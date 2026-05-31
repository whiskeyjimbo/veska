//go:build eval

// Eval harness for solov2-7ma: measures recall@10 + p95 for each
// embed-text projection variant so a reference-laptop run can decide
// whether folding signature and/or a code snippet into the embed text
// improves embedding quality.
//
// The corpus is built from node-shaped projection inputs run through
// domain.EmbedText — the SAME projection production FetchPending uses —
// so a variant change is exactly what the recall delta measures.
//
// Build-tag-gated (`eval`) like the sibling harnesses; the make target is
// `make eval-recall-projection`. The test SKIPS with a clear message when
// Ollama is unreachable rather than failing.
package recallprojection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/recall"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

const (
	defaultOllamaURL   = "http://localhost:11434"
	defaultOllamaModel = "nomic-embed-text"
	recallK            = 10
)

// variantResult is one row of the projection sweep.
type variantResult struct {
	Variant      string  `json:"variant"`
	Population   int     `json:"population"`
	Clusters     int     `json:"clusters"`
	Queries      int     `json:"queries"`
	MeanRecall   float64 `json:"mean_recall"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
	Embedder     string  `json:"embedder"`
	Backend      string  `json:"backend"`
}

// TestRecallProjectionSweep runs the recall harness once per projection
// variant (baseline / +signature / +snippet / +both) so the measured
// recall numbers can be compared. With RECALL_PROJECTION_VARIANT set, only
// that single variant runs.
//
// Env knobs:
//   - RECALL_POP                  synthetic-corpus population (default 1000)
//   - RECALL_PROJECTION_CORPUS    "real:<path>" builds the corpus from a
//     real Go module (faithful snippet/query; solov2-ok0); unset uses the
//     synthetic corpus
//   - RECALL_PROJECTION_VARIANT   restrict to one variant (baseline|
//     +signature|+snippet|+both); unset sweeps all four
//   - VESKA_OLLAMA_URL            Ollama base URL (probe + embed)
//   - VESKA_EMBED_MODEL           embedding model
//   - VESKA_VECTOR_BACKEND        vector backend (default sqlite-vec)
func TestRecallProjectionSweep(t *testing.T) {
	var corpus ProjectionCorpus
	pop := envInt("RECALL_POP", 1000)

	if spec := os.Getenv("RECALL_PROJECTION_CORPUS"); strings.HasPrefix(spec, "real:") {
		// Faithful corpus: real source bodies (snippet) and doc-comment
		// queries written independently of them — no synthSnippet circularity.
		path := strings.TrimPrefix(spec, "real:")
		rc, err := BuildRealCorpus(path)
		if err != nil {
			t.Fatalf("BuildRealCorpus(%s): %v", path, err)
		}
		if len(rc.Nodes) == 0 || len(rc.CenterQueries) == 0 {
			t.Fatalf("real corpus at %s has no documented symbols to query", path)
		}
		corpus = rc
		pop = len(rc.Nodes)
	} else {
		clusters := synthcorpus.SemanticClusterCount
		nodesPerCluster := pop / clusters
		if nodesPerCluster < 1 {
			t.Fatalf("RECALL_POP=%d too small: need at least %d (clusters)", pop, clusters)
		}
		pop = clusters * nodesPerCluster

		// The semantic corpus carries disjoint per-cluster topic
		// vocabularies, so a real embedding model can separate clusters —
		// required for the projection delta to be visible above noise.
		src := synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
		corpus = BuildProjectionCorpus(src)
	}

	ollamaURL := envStr("VESKA_OLLAMA_URL", defaultOllamaURL)
	model := envStr("VESKA_EMBED_MODEL", defaultOllamaModel)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	if err := probeOllama(probeCtx, ollamaURL); err != nil {
		t.Skipf("recallprojection: Ollama not reachable at %s (%v) — skipping projection sweep",
			ollamaURL, err)
		return
	}
	provider, err := ollama.New(model, ollama.WithBaseURL(ollamaURL))
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}

	variants := AllVariants
	if v := os.Getenv("RECALL_PROJECTION_VARIANT"); v != "" {
		variants = []domain.EmbedTextVariant{VariantByName(v)}
	}

	results := make([]variantResult, 0, len(variants))
	for _, variant := range variants {
		res := runVariant(t, provider, corpus, variant, pop, model)
		results = append(results, res)
		fmt.Printf("RECALL_PROJECTION variant=%s pop=%d mean_recall=%.4f p95_latency_ms=%.2f embedder=%s backend=%s\n",
			res.Variant, res.Population, res.MeanRecall, res.P95LatencyMs, res.Embedder, res.Backend)
	}

	if err := writeJSON("recall_projection_results.json", results); err != nil {
		t.Logf("writeJSON: %v (continuing)", err)
	}
}

// runVariant builds the embedding fixture for one projection variant,
// loads it into a real VectorStorage + SQLite, and drives center queries
// through the real search.Service.
func runVariant(
	t *testing.T,
	provider interface {
		Embed(context.Context, string) ([]float32, error)
		ModelID() string
	},
	corpus ProjectionCorpus,
	variant domain.EmbedTextVariant,
	pop int,
	model string,
) variantResult {
	t.Helper()
	ctx := context.Background()

	// Embed every node via its projection under this variant.
	var (
		dim     int
		vectors []float32
	)
	for i, n := range corpus.Nodes {
		text := n.EmbedText(variant)
		vec, err := provider.Embed(ctx, text)
		if err != nil {
			t.Fatalf("variant %s: embed node %d (%s): %v", variant, i, n.NodeID, err)
		}
		if i == 0 {
			dim = len(vec)
			if dim <= 0 {
				t.Fatalf("variant %s: provider returned dim=%d", variant, dim)
			}
			vectors = make([]float32, 0, dim*len(corpus.Nodes))
		} else if len(vec) != dim {
			t.Fatalf("variant %s: dim drift at node %d: got %d want %d", variant, i, len(vec), dim)
		}
		l2Normalize(vec)
		vectors = append(vectors, vec...)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(tmpDir, "backups")})
	if err != nil {
		t.Fatalf("variant %s: sqlite.OpenWithOptions: %v", variant, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		repoID = "recall-projection-eval"
		branch = "main"
	)
	seedNodes(t, db, repoID, branch, corpus.Nodes)

	backendKind := vector.BackendKind(os.Getenv("VESKA_VECTOR_BACKEND"))
	if backendKind == "" {
		backendKind = vector.BackendSQLiteVec
	}
	vstore, err := vector.NewVectorStorage(backendKind, t.TempDir())
	if err != nil {
		t.Fatalf("variant %s: vector.NewVectorStorage(%s): %v", variant, backendKind, err)
	}

	rows := make([]domain.EmbeddingRow, len(corpus.Nodes))
	for i, n := range corpus.Nodes {
		rows[i] = domain.EmbeddingRow{
			NodeID:      n.NodeID,
			ContentHash: "h-" + n.NodeID,
			ModelID:     provider.ModelID(),
			Vector:      append([]float32(nil), vectors[i*dim:(i+1)*dim]...),
		}
	}
	if err := vstore.UpsertEmbeddings(ctx, repoID, branch, rows); err != nil {
		t.Fatalf("variant %s: UpsertEmbeddings: %v", variant, err)
	}

	nodeLookup := sqlite.NewNodeLookupRepo(db)
	svc := search.NewService(provider, vstore, nodeLookup)

	truth := corpus.TruthByCluster()
	perQuery := make([]float64, 0, corpus.Clusters)
	latencies := make([]time.Duration, 0, corpus.Clusters)
	for cluster, q := range corpus.CenterQueries {
		start := time.Now()
		resp, err := svc.Semantic(ctx, repoID, branch, q, recallK, domain.VectorFilter{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("variant %s: Semantic(cluster %d): %v", variant, cluster, err)
		}
		ids := make([]string, len(resp.Results))
		for i, r := range resp.Results {
			ids[i] = r.NodeID
		}
		perQuery = append(perQuery, recall.RecallAtK(ids, truth[cluster], recallK))
		latencies = append(latencies, elapsed)
	}

	p95 := recall.P95Latency(latencies)
	return variantResult{
		Variant:      variant.String(),
		Population:   pop,
		Clusters:     corpus.Clusters,
		Queries:      len(corpus.CenterQueries),
		MeanRecall:   recall.MeanRecall(perQuery),
		P95LatencyMs: float64(p95.Microseconds()) / 1000.0,
		Embedder:     "ollama:" + model,
		Backend:      string(backendKind),
	}
}

// seedNodes inserts the projection corpus into the nodes table so the
// NodeLookupRepo can hydrate IDs returned by VectorStorage.Search.
func seedNodes(t *testing.T, db *sql.DB, repoID, branch string, nodes []ProjectionNode) {
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
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for _, n := range nodes {
		if _, err := stmt.Exec(
			n.NodeID, branch, repoID, n.Input.Language, n.Input.Kind,
			n.Input.SymbolPath, n.Input.FilePath, 1, 1, "h-"+n.NodeID, now,
			"recall-projection-eval", "system",
		); err != nil {
			t.Fatalf("insert node %s: %v", n.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// l2Normalize scales v to unit L2 norm in place; a zero vector is left
// unchanged. Matches the recall harness's GenerateOllamaFixture.
func l2Normalize(v []float32) {
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	if sq == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sq))
	for i := range v {
		v[i] *= inv
	}
}

func probeOllama(ctx context.Context, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
