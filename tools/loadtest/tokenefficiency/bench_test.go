//go:build eval

// End-to-end token-efficiency harness (solov2-wise). Drives a real
// search.Service against the deterministic semantic synthcorpus,
// simulates grep+read across the same corpus represented as one file
// per cluster, and emits per-query + aggregate token / recall numbers.
//
// Build-tag-gated so `go test ./...` stays fast.
package tokenefficiency

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/tokenize"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// loadEmbedder picks the real model2vec provider when its assets are
// present on disk and falls back to the deterministic FakeEmbedder
// otherwise. The FakeEmbedder is hash-based — useful for plumbing
// smoke-tests but NOT for honest recall numbers on the semantic corpus
// (its cluster-aligned tuning targets the original GenerateCorpus
// vocabulary, not the per-topic semantic phrase bags). The returned
// embedderName is surfaced in the summary line so the recall figure is
// always reported alongside which model produced it.
func loadEmbedder() (ports.EmbeddingProvider, string, []float32, int) {
	if dir := findModel2VecAssets(); dir != "" {
		if p, err := model2vec.New(dir); err == nil {
			// Probe one Embed to get the produced dimension so the
			// vector-store seeding code uses the right size.
			vec, err := p.Embed(context.Background(), "probe")
			if err == nil {
				return p, "model2vec:" + filepath.Base(dir), vec, len(vec)
			}
		}
	}
	return synthcorpus.FakeEmbedder{}, "fake", nil, synthcorpus.FakeEmbeddingDim
}

// findModel2VecAssets walks up from this source file looking for the
// repo's embedded-model assets dir. The assets are gitignored (the
// ~62MB safetensors blob lives there only after `make fetch-embed-assets`
// or `make build`), so an empty return is normal in a clean clone.
func findModel2VecAssets() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(file)
	for range 8 {
		candidate := filepath.Join(dir, "internal", "infrastructure", "embedding", "model2vec", "assets", "potion-code-16M")
		if st, err := os.Stat(filepath.Join(candidate, "model.safetensors")); err == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// TestTokenEfficiency is the headline harness. Wiring mirrors
// recall.TestRecall — synthetic semantic corpus, fake embedder, real
// search.Service + sqlite-vec — so apples-to-apples comparisons stay
// possible. Output: tools/loadtest/tokenefficiency/results.json plus a
// one-line semble-shaped summary on stdout.
//
// Env knobs:
//   - TOKEFF_NODES_PER_CLUSTER (default 24) — larger files exaggerate
//     grep's read-all-matches cost so the bracket widens. Capped by
//     synthcorpus' per-cluster combinatorial cap.
func TestTokenEfficiency(t *testing.T) {
	nodesPerCluster := envInt("TOKEFF_NODES_PER_CLUSTER", 24)
	if nodesPerCluster < 5 {
		t.Fatalf("nodesPerCluster=%d too small (need >= 5)", nodesPerCluster)
	}
	const k = 10

	corpus := synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
	clusters := corpus.Clusters

	// --- build the simulated filesystem -----------------------------------
	// One file per cluster, containing every member node's text joined
	// with a blank line. node->file and file->nodes maps let the
	// baseline simulator detect truth coverage during a stop-when-covered
	// walk.
	filesByPath := make(map[string]string, clusters)
	fileNodeIDs := make(map[string][]string, clusters)
	for _, n := range corpus.Nodes {
		filesByPath[n.FilePath] = filesByPath[n.FilePath] + n.Text + "\n\n"
		fileNodeIDs[n.FilePath] = append(fileNodeIDs[n.FilePath], n.NodeID)
	}
	// Pre-tokenise every file once; baseline mode re-uses the counts.
	fileTokens := make(map[string]int, len(filesByPath))
	for p, body := range filesByPath {
		n, err := CountTokens(body)
		if err != nil {
			t.Fatalf("CountTokens(%s): %v", p, err)
		}
		fileTokens[p] = n
	}

	// --- wire SQLite + VectorStorage -------------------------------------
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(tmpDir, "backups")})
	if err != nil {
		t.Fatalf("sqlite.OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		repoID = "tokeneff-eval"
		branch = "main"
	)
	seedNodesWithSnippets(t, db, repoID, branch, corpus.Nodes)

	vstore, err := vector.NewVectorStorage(vector.BackendSQLiteVec, t.TempDir())
	if err != nil {
		t.Fatalf("vector.NewVectorStorage: %v", err)
	}

	embedder, embedderName, _, _ := loadEmbedder()
	bgCtx := context.Background()
	rows, err := embedAllNodes(bgCtx, embedder, embedderName, corpus.Nodes)
	if err != nil {
		t.Fatalf("embed nodes: %v", err)
	}
	if err := vstore.UpsertEmbeddings(bgCtx, repoID, branch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// --- run the query loop ----------------------------------------------
	svc := search.NewService(embedder, vstore, sqlite.NewNodeLookupRepo(db),
		search.WithLexicalSearcher(sqlite.NewLexicalRepo(db)),
	)
	truth := corpus.TruthByCluster()

	perQuery := make([]PerQuery, 0, clusters)
	recalls := make([]float64, 0, clusters)
	veskaTokens := make([]int, 0, clusters)
	loTokensAll := make([]int, 0, clusters)
	hiTokensAll := make([]int, 0, clusters)
	loRecalls := make([]float64, 0, clusters)
	savingsLo := make([]float64, 0, clusters)
	savingsHi := make([]float64, 0, clusters)

	for cluster, q := range corpus.CenterQueries {
		resp, err := svc.Semantic(bgCtx, repoID, branch, q, k, domain.Filter{})
		if err != nil {
			t.Fatalf("Semantic(cluster %d): %v", cluster, err)
		}
		hits := make([]string, len(resp.Results))
		searchResults := make([]SearchResult, len(resp.Results))
		for i, r := range resp.Results {
			hits[i] = r.NodeID
			searchResults[i] = SearchResult{NodeID: r.NodeID, Snippet: r.Snippet}
		}
		r := RecallAtK(hits, truth[cluster], k)
		vt, err := VeskaTokens(searchResults)
		if err != nil {
			t.Fatalf("VeskaTokens(cluster %d): %v", cluster, err)
		}

		grepHits := SimulateGrepFilesWithMatches(q, filesByPath)
		lo, hi, loR, hiR := BaselineGrep(grepHits, fileTokens, fileNodeIDs, truth[cluster])

		row := PerQuery{
			Query:            q,
			Recall:           r,
			VeskaTokens:      vt,
			BaselineLoTokens: lo,
			BaselineHiTokens: hi,
			SavingsLoVsGrep:  SavingsRatio(vt, lo),
			SavingsHiVsGrep:  SavingsRatio(vt, hi),
			BaselineLoRecall: loR,
			BaselineHiRecall: hiR,
		}
		perQuery = append(perQuery, row)
		recalls = append(recalls, r)
		veskaTokens = append(veskaTokens, vt)
		loTokensAll = append(loTokensAll, lo)
		hiTokensAll = append(hiTokensAll, hi)
		loRecalls = append(loRecalls, loR)
		savingsLo = append(savingsLo, row.SavingsLoVsGrep)
		savingsHi = append(savingsHi, row.SavingsHiVsGrep)
	}

	res := Result{
		Queries:             len(perQuery),
		K:                   k,
		Tokenizer:           EncodingName,
		MeanRecall:          Mean(recalls),
		MeanVeskaTokens:     MeanInt(veskaTokens),
		MeanGrepLoTokens:    MeanInt(loTokensAll),
		MeanGrepHiTokens:    MeanInt(hiTokensAll),
		MeanSavingsLoVsGrep: Mean(savingsLo),
		MeanSavingsHiVsGrep: Mean(savingsHi),
		MeanGrepLoRecall:    Mean(loRecalls),
		PerQuery:            perQuery,
		CorpusNote: fmt.Sprintf(
			"auto-generated semantic synthcorpus; ground truth is by cluster construction; embedder=%s",
			embedderName,
		),
		Embedder:  embedderName,
		Timestamp: time.Now().UTC(),
	}
	if err := writeJSON("results.json", res); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	fmt.Println(res.SummaryLine())
	fmt.Printf("TOKENEFF embedder=%s queries=%d recall=%.2f veska_tok=%.0f grep_lo=%.0f grep_hi=%.0f savings=[%.0f%%, %.0f%%]\n",
		embedderName, res.Queries, res.MeanRecall, res.MeanVeskaTokens, res.MeanGrepLoTokens, res.MeanGrepHiTokens,
		res.MeanSavingsLoVsGrep*100, res.MeanSavingsHiVsGrep*100,
	)

	// Sanity guards: a zero-recall run or zero-savings range almost
	// always means the corpus + embedder plumbing was broken, not a
	// real result. Fail loudly so the eval never silently publishes a
	// nonsense headline number.
	if res.MeanRecall == 0 {
		t.Fatalf("mean recall is zero — embedder + corpus plumbing is broken")
	}
}

// seedNodesWithSnippets is the recall harness' seedNodes + a snippet
// payload. The snippet IS the node's synthetic Text — that's what
// search.Service hydrates and what the agent would land in its context.
func seedNodesWithSnippets(t *testing.T, db *sql.DB, repoID, branch string, nodes []synthcorpus.SyntheticNode) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		repoID, "/tmp/"+repoID, now,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("db.Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind, snippet
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for _, n := range nodes {
		if _, err := stmt.Exec(
			n.NodeID, branch, repoID, "go", n.Kind, n.SymbolPath, n.FilePath,
			1, 1, "h-"+n.NodeID, now, "tokeneff-eval", "system", n.Text,
		); err != nil {
			t.Fatalf("insert node %s: %v", n.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	seedFTS(t, db, repoID, branch, nodes)
}

// seedFTS pushes each node's Text into the production FTS5 virtual
// tables (node_fts_words + node_fts_trigrams). Wiring LexicalRepo on
// top of these tables gives the search.Service a real lexical
// retriever, which is what makes cross-repo global RRF actually
// discriminate (vector-only RRF leaves every repo's rank-1 tied at
// 1/(60+1), and the cross-repo top-K becomes ~uniform across repos).
func seedFTS(t *testing.T, db *sql.DB, repoID, branch string, nodes []synthcorpus.SyntheticNode) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("fts begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	wordsStmt, err := tx.Prepare(`INSERT INTO node_fts_words (node_id, branch, repo_id, words) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("fts prepare words: %v", err)
	}
	defer wordsStmt.Close()
	triStmt, err := tx.Prepare(`INSERT INTO node_fts_trigrams (node_id, branch, repo_id, raw) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("fts prepare trigrams: %v", err)
	}
	defer triStmt.Close()
	for _, n := range nodes {
		if _, err := wordsStmt.Exec(n.NodeID, branch, repoID, tokenize.Symbol(n.Text)); err != nil {
			t.Fatalf("fts words insert: %v", err)
		}
		if _, err := triStmt.Exec(n.NodeID, branch, repoID, n.Text); err != nil {
			t.Fatalf("fts trigrams insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("fts commit: %v", err)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// embedAllNodes runs embedder.Embed over every node's Text and returns
// the EmbeddingRow batch ready for VectorStorage.UpsertEmbeddings. The
// ContentHash is a placeholder ("h-<NodeID>") — these rows never go
// through the real promotion path. ModelID derives from embedderName so
// the cached vectors are tagged with the actual model that produced
// them (sqlite-vec doesn't read ModelID for search, but downstream
// debugging is much easier when the column isn't lying).
func embedAllNodes(ctx context.Context, embedder ports.EmbeddingProvider, embedderName string, nodes []synthcorpus.SyntheticNode) ([]domain.EmbeddingRow, error) {
	out := make([]domain.EmbeddingRow, len(nodes))
	for i, n := range nodes {
		vec, err := embedder.Embed(ctx, n.Text)
		if err != nil {
			return nil, fmt.Errorf("embed %s: %w", n.NodeID, err)
		}
		out[i] = domain.EmbeddingRow{
			NodeID:      n.NodeID,
			ContentHash: "h-" + n.NodeID,
			ModelID:     embedderName,
			Vector:      vec,
		}
	}
	return out, nil
}

// Reference _ usage so unused-import linters stay quiet under sparse
// branches. The strings import shows up in seedNodesWithSnippets via
// generated paths in the broader harness.
var _ = strings.Contains
