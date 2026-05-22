//go:build eval

// Eval harness for solov2-hd0: the DECISION GATE for making model2vec
// (potion-code-16M) the default embedder over Ollama (nomic-embed-text).
//
// It runs the SAME corpus and the SAME query set through two embedders
// and reports recall@10, recall@5, MRR, and p95 embed latency for each:
//
//   - Ollama nomic-embed-text  (the incumbent default)
//   - model2vec potion-code-16M (the static candidate)
//
// Both arms reuse the projection corpus + production EmbedText projection
// (default EmbedVariantSnippet, the variant the sqlite adapter uses) and
// the real search.Service with NO lexical searcher wired, so the ranking
// is pure vector cosine — the comparison isolates embedding quality, with
// no FTS fusion masking the embedder delta.
//
// Decision rule (printed as a verdict at the end):
//   - model2vec recall@10 within ~10% of Ollama AND p95 embed latency
//     comparable-or-better  ⇒  model2vec becomes the default.
//   - materially worse                                    ⇒  keep Ollama.
//
// The make target is `make eval-embedder-recall`. Each arm SKIPS with a
// clear message when its dependency is missing (Ollama unreachable, or the
// model2vec model files absent under VESKA_HOME/static-model/<model>).
package recallprojection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/tools/loadtest/recall"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// recallTolerance is the fraction by which model2vec recall@10 may trail
// Ollama and still be declared "competitive" per the solov2-hd0 rule.
const recallTolerance = 0.10

// embedProvider is the slice of the EmbeddingProvider port this harness
// drives. Both ollama.Provider and model2vec.Provider satisfy it.
type embedProvider interface {
	Embed(context.Context, string) ([]float32, error)
	ModelID() string
}

// embedderResult is one arm of the comparison.
type embedderResult struct {
	Embedder          string  `json:"embedder"`
	Population        int     `json:"population"`
	Queries           int     `json:"queries"`
	Variant           string  `json:"variant"`
	Backend           string  `json:"backend"`
	RecallAt10        float64 `json:"recall_at_10"`
	RecallAt5         float64 `json:"recall_at_5"`
	MRR               float64 `json:"mrr"`
	P95EmbedLatencyMs float64 `json:"p95_embed_latency_ms"`
}

// TestEmbedderRecallCompare is the model2vec-vs-Ollama decision gate.
//
// Env knobs (shared with the projection sweep where they overlap):
//   - RECALL_POP                  synthetic-corpus population (default 1000)
//   - RECALL_PROJECTION_CORPUS    "real:<path>" for a real Go module corpus
//   - RECALL_PROJECTION_VARIANT   projection variant (default +snippet)
//   - VESKA_OLLAMA_URL            Ollama base URL
//   - VESKA_EMBED_MODEL           Ollama embedding model (default nomic-embed-text)
//   - VESKA_HOME                  data root holding static-model/<model>/
//   - VESKA_STATIC_MODEL          model2vec model dir name (default potion-code-16M)
//   - VESKA_VECTOR_BACKEND        vector backend (default sqlite-vec)
func TestEmbedderRecallCompare(t *testing.T) {
	corpus, pop := buildCompareCorpus(t)

	variant := domain.EmbedVariantSnippet
	if v := os.Getenv("RECALL_PROJECTION_VARIANT"); v != "" {
		variant = VariantByName(v)
	}

	// --- Arm 1: Ollama nomic-embed-text ---
	ollamaURL := envStr("VESKA_OLLAMA_URL", defaultOllamaURL)
	model := envStr("VESKA_EMBED_MODEL", defaultOllamaModel)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	if err := probeOllama(probeCtx, ollamaURL); err != nil {
		t.Skipf("embedder-compare: Ollama not reachable at %s (%v) — cannot run the decision gate", ollamaURL, err)
		return
	}
	ollamaProvider, err := ollama.New(model, ollama.WithBaseURL(ollamaURL))
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}
	ollamaRes := runEmbedder(t, ollamaProvider, "ollama:"+model, corpus, variant, pop)

	// --- Arm 2: model2vec potion-code-16M ---
	veskaHome := envStr("VESKA_HOME", filepath.Join(os.Getenv("HOME"), ".veska"))
	modelName := envStr("VESKA_STATIC_MODEL", "potion-code-16M")
	m2v, err := model2vec.TryLoad(veskaHome, modelName)
	if err != nil {
		t.Skipf("embedder-compare: model2vec model %q not loadable under %s (%v) — "+
			"place tokenizer.json + model.safetensors there to run the gate",
			modelName, filepath.Join(veskaHome, "static-model", modelName), err)
		return
	}
	m2vRes := runEmbedder(t, m2v, "model2vec:"+modelName, corpus, variant, pop)

	// --- Report + verdict ---
	results := []embedderResult{ollamaRes, m2vRes}
	for _, r := range results {
		fmt.Printf("EMBEDDER_COMPARE embedder=%s pop=%d queries=%d variant=%s "+
			"recall@10=%.4f recall@5=%.4f mrr=%.4f p95_embed_ms=%.2f backend=%s\n",
			r.Embedder, r.Population, r.Queries, r.Variant,
			r.RecallAt10, r.RecallAt5, r.MRR, r.P95EmbedLatencyMs, r.Backend)
	}
	if err := writeJSON("embedder_compare_results.json", results); err != nil {
		t.Logf("writeJSON: %v (continuing)", err)
	}
	printVerdict(t, ollamaRes, m2vRes)
}

// buildCompareCorpus assembles the shared corpus exactly as the projection
// sweep does, so both harnesses measure the same population.
func buildCompareCorpus(t *testing.T) (ProjectionCorpus, int) {
	t.Helper()
	pop := envInt("RECALL_POP", 1000)
	if spec := os.Getenv("RECALL_PROJECTION_CORPUS"); strings.HasPrefix(spec, "real:") {
		path := strings.TrimPrefix(spec, "real:")
		rc, err := BuildRealCorpus(path)
		if err != nil {
			t.Fatalf("BuildRealCorpus(%s): %v", path, err)
		}
		if len(rc.Nodes) == 0 || len(rc.CenterQueries) == 0 {
			t.Fatalf("real corpus at %s has no documented symbols to query", path)
		}
		return rc, len(rc.Nodes)
	}
	clusters := synthcorpus.SemanticClusterCount
	nodesPerCluster := pop / clusters
	if nodesPerCluster < 1 {
		t.Fatalf("RECALL_POP=%d too small: need at least %d (clusters)", pop, clusters)
	}
	pop = clusters * nodesPerCluster
	src := synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
	return BuildProjectionCorpus(src), pop
}

// runEmbedder embeds the corpus through provider, indexes it, drives the
// center queries through a real (vector-only) search.Service, and returns
// recall@10, recall@5, MRR, and p95 per-embed-call latency.
func runEmbedder(
	t *testing.T,
	provider embedProvider,
	name string,
	corpus ProjectionCorpus,
	variant domain.EmbedTextVariant,
	pop int,
) embedderResult {
	t.Helper()
	ctx := context.Background()

	var (
		dim       int
		vectors   []float32
		embedLats []time.Duration
	)
	for i, n := range corpus.Nodes {
		text := n.EmbedText(variant)
		start := time.Now()
		vec, err := provider.Embed(ctx, text)
		embedLats = append(embedLats, time.Since(start))
		if err != nil {
			t.Fatalf("%s: embed node %d (%s): %v", name, i, n.NodeID, err)
		}
		if i == 0 {
			dim = len(vec)
			if dim <= 0 {
				t.Fatalf("%s: provider returned dim=%d", name, dim)
			}
			vectors = make([]float32, 0, dim*len(corpus.Nodes))
		} else if len(vec) != dim {
			t.Fatalf("%s: dim drift at node %d: got %d want %d", name, i, len(vec), dim)
		}
		l2Normalize(vec)
		vectors = append(vectors, vec...)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: filepath.Join(tmpDir, "backups")})
	if err != nil {
		t.Fatalf("%s: sqlite.OpenWithOptions: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		repoID = "embedder-compare-eval"
		branch = "main"
	)
	seedNodes(t, db, repoID, branch, corpus.Nodes)

	backendKind := vector.BackendKind(os.Getenv("VESKA_VECTOR_BACKEND"))
	if backendKind == "" {
		backendKind = vector.BackendSQLiteVec
	}
	vstore, err := vector.NewVectorStorage(backendKind, t.TempDir())
	if err != nil {
		t.Fatalf("%s: vector.NewVectorStorage(%s): %v", name, backendKind, err)
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
		t.Fatalf("%s: UpsertEmbeddings: %v", name, err)
	}

	nodeLookup := sqlite.NewNodeLookupRepo(db)
	svc := search.NewService(provider, vstore, nodeLookup)

	truth := corpus.TruthByCluster()
	recall10 := make([]float64, 0, corpus.Clusters)
	recall5 := make([]float64, 0, corpus.Clusters)
	rr := make([]float64, 0, corpus.Clusters)
	for cluster, q := range corpus.CenterQueries {
		start := time.Now()
		resp, err := svc.Semantic(ctx, repoID, branch, q, recallK, domain.Filter{})
		embedLats = append(embedLats, time.Since(start)) // query embed+search; embed dominates for static
		if err != nil {
			t.Fatalf("%s: Semantic(cluster %d): %v", name, cluster, err)
		}
		ids := make([]string, len(resp.Results))
		for i, r := range resp.Results {
			ids[i] = r.NodeID
		}
		recall10 = append(recall10, recall.RecallAtK(ids, truth[cluster], recallK))
		recall5 = append(recall5, recall.RecallAtK(ids, truth[cluster], 5))
		rr = append(rr, recall.ReciprocalRank(ids, truth[cluster]))
	}

	return embedderResult{
		Embedder:          name,
		Population:        pop,
		Queries:           len(corpus.CenterQueries),
		Variant:           variant.String(),
		Backend:           string(backendKind),
		RecallAt10:        recall.MeanRecall(recall10),
		RecallAt5:         recall.MeanRecall(recall5),
		MRR:               recall.MRR(rr),
		P95EmbedLatencyMs: float64(recall.P95Latency(embedLats).Microseconds()) / 1000.0,
	}
}

// printVerdict applies the solov2-hd0 decision rule and prints the
// resulting recommendation. It does not fail the test — the gate is a
// measurement, not a pass/fail assertion; a human reads the verdict.
func printVerdict(t *testing.T, ollamaRes, m2vRes embedderResult) {
	t.Helper()
	var recallGap float64
	if ollamaRes.RecallAt10 > 0 {
		recallGap = (ollamaRes.RecallAt10 - m2vRes.RecallAt10) / ollamaRes.RecallAt10
	}
	recallOK := recallGap <= recallTolerance
	latencyOK := m2vRes.P95EmbedLatencyMs <= ollamaRes.P95EmbedLatencyMs

	verdict := "KEEP OLLAMA as default (model2vec materially worse)"
	if recallOK && latencyOK {
		verdict = "MAKE MODEL2VEC the default (competitive recall + comparable-or-better latency)"
	} else if recallOK && !latencyOK {
		verdict = "model2vec recall competitive but latency worse — investigate before switching"
	}

	fmt.Printf("EMBEDDER_COMPARE_VERDICT recall_gap=%.1f%% (tol=%.0f%%) recall_ok=%v latency_ok=%v => %s\n",
		recallGap*100, recallTolerance*100, recallOK, latencyOK, verdict)
}
