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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

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
	pop := len(corpus.Nodes)
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

	dim := synthcorpus.FakeEmbeddingDim
	rows := make([]domain.EmbeddingRow, pop)
	for i, n := range corpus.Nodes {
		rows[i] = domain.EmbeddingRow{
			NodeID:      n.NodeID,
			ContentHash: "h-" + n.NodeID,
			ModelID:     "fake-hash-v1",
			Vector:      append([]float32(nil), synthcorpus.FakeEmbed(n.Text)...),
		}
	}
	_ = dim
	if err := vstore.UpsertEmbeddings(context.Background(), repoID, branch, rows); err != nil {
		t.Fatalf("UpsertEmbeddings: %v", err)
	}

	// --- run the query loop ----------------------------------------------
	svc := search.NewService(synthcorpus.FakeEmbedder{}, vstore, sqlite.NewNodeLookupRepo(db))
	truth := corpus.TruthByCluster()

	perQuery := make([]PerQuery, 0, clusters)
	recalls := make([]float64, 0, clusters)
	veskaTokens := make([]int, 0, clusters)
	loTokensAll := make([]int, 0, clusters)
	hiTokensAll := make([]int, 0, clusters)
	loRecalls := make([]float64, 0, clusters)
	savingsLo := make([]float64, 0, clusters)
	savingsHi := make([]float64, 0, clusters)

	ctx := context.Background()
	for cluster, q := range corpus.CenterQueries {
		resp, err := svc.Semantic(ctx, repoID, branch, q, k, domain.Filter{})
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
		CorpusNote:          "auto-generated semantic synthcorpus; ground truth is by cluster construction (biases recall up vs human-annotated corpora)",
		Timestamp:           time.Now().UTC(),
	}
	if err := writeJSON("results.json", res); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	fmt.Println(res.SummaryLine())
	fmt.Printf("TOKENEFF queries=%d recall=%.2f veska_tok=%.0f grep_lo=%.0f grep_hi=%.0f savings=[%.0f%%, %.0f%%]\n",
		res.Queries, res.MeanRecall, res.MeanVeskaTokens, res.MeanGrepLoTokens, res.MeanGrepHiTokens,
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

// Reference _ usage so unused-import linters stay quiet under sparse
// branches. The strings import shows up in seedNodesWithSnippets via
// generated paths in the broader harness.
var _ = strings.Contains
