//go:build eval

// Package embed_models benchmarks embedding model variants — model2vec
// in-process providers and (in later phases) Ollama models — over real
// codebase corpora, so hi5's defaults are data-backed and end users get
// a published comparison table (solov2-0k5h).
//
// Phase 0k5h.2 — multi-model, multi-corpus, structured output.
// Iterates over every model under $VESKA_HOME/static-model/<name>/ that
// matches the BenchModels list, and every corpus directory that's been
// fetched into out/repos/<name>/ (via scripts/fetch-corpora.sh) plus the
// always-present veska self-corpus. Models / corpora that aren't on disk
// are skipped with a warning — the bench is runnable with whatever
// subset is installed.
//
// Run with: make eval-embed-models
//   Env knobs:
//     EMBED_BENCH_MODEL_DIR  — override the model search path (default:
//                              $VESKA_HOME/static-model)
//     EMBED_BENCH_QUERY      — query string used for the printed top-K
//                              sanity check (default: "load config")
//     EMBED_BENCH_TOPK       — number of top results to print (default 10)
//     EMBED_BENCH_MAX_DOCS   — cap docs per corpus to bound runtime
//                              during iteration (default 5000)
//     EMBED_BENCH_OUT        — path to write results JSON
//                              (default: tools/loadtest/embed_models/out/results.json)
package embed_models

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// BenchModels lists the model2vec variants the bench targets (0k5h.2).
// Order is footprint-ascending so output is easier to scan.
var BenchModels = []string{
	"potion-base-2M",
	"potion-base-8M",
	"potion-code-16M",
	"potion-retrieval-32M",
	"potion-base-32M",
	"potion-base-128M",
}

// BenchCorpora lists the corpus names the bench targets. veska is
// always present (this repo); the rest live under out/repos/<name>/
// after scripts/fetch-corpora.sh runs.
var BenchCorpora = []string{"veska", "cobra", "pflag", "testify", "gin"}

// doc is one embedded corpus document.
type doc struct {
	name string
	file string
	vec  []float32
}

// modelEntry pairs a model name with its on-disk dir, used for
// iteration. Resolved at test start; missing models drop out of the
// list with a logged warning.
type modelEntry struct {
	name string
	dir  string
}

// corpusEntry pairs a corpus name with its root directory.
type corpusEntry struct {
	name string
	root string
}

// runResult captures per-(model × corpus) bench data — what gets
// written to results.json and consumed by 0k5h.6's table generator.
type runResult struct {
	Model       string    `json:"model"`
	Corpus      string    `json:"corpus"`
	DocCount    int       `json:"doc_count"`
	EmbedTotal  string    `json:"embed_total"`     // human-readable duration
	EmbedTotalMS float64  `json:"embed_total_ms"`  // machine-readable
	EmbedAvgMS  float64   `json:"embed_avg_ms"`
	QueryMS     float64   `json:"query_ms"`        // query embed time
	TopHits     []topHit  `json:"top_hits"`        // sanity-check top-K for the printed query
}

type topHit struct {
	Name  string  `json:"name"`
	File  string  `json:"file"`
	Score float32 `json:"score"`
}

// benchResults is the on-disk JSON shape — a phase number + every
// per-run row. Later phases (0k5h.3 recall, 0k5h.6 table) read it.
type benchResults struct {
	Phase    string      `json:"phase"`
	GeneratedAt string   `json:"generated_at"`
	Runs     []runResult `json:"runs"`
}

func TestEmbedModelsBenchmark(t *testing.T) {
	models := discoverModels(t)
	if len(models) == 0 {
		t.Fatalf("no model2vec models found under %s; run scripts/install-bench-models.sh", modelRoot())
	}

	corpora := discoverCorpora(t)
	if len(corpora) == 0 {
		t.Fatalf("no corpora available (veska self-corpus should always resolve)")
	}

	query := envOr("EMBED_BENCH_QUERY", "load config")
	topK := envInt("EMBED_BENCH_TOPK", 10)
	maxDocs := envInt("EMBED_BENCH_MAX_DOCS", 5000)

	t.Logf("models found: %d (%v)", len(models), modelNames(models))
	t.Logf("corpora found: %d (%v)", len(corpora), corpusNames(corpora))
	t.Logf("query=%q topK=%d max_docs=%d", query, topK, maxDocs)

	var results []runResult
	for _, m := range models {
		provider, err := model2vec.New(m.dir)
		if err != nil {
			t.Logf("WARN: load %s: %v — skipping", m.name, err)
			continue
		}
		for _, c := range corpora {
			t.Logf("--- run: model=%s corpus=%s ---", m.name, c.name)
			docs, stats := embedCorpus(t, provider, c.root, maxDocs)
			if len(docs) == 0 {
				t.Logf("  no docs from %s — skip", c.root)
				continue
			}
			qStart := time.Now()
			qvec, err := provider.Embed(context.Background(), query)
			if err != nil {
				t.Logf("  embed query failed: %v — skip", err)
				continue
			}
			qElapsed := time.Since(qStart)
			hits := rank(qvec, docs)
			if topK > len(hits) {
				topK = len(hits)
			}
			top := make([]topHit, 0, topK)
			for i := 0; i < topK; i++ {
				rel, _ := filepath.Rel(c.root, hits[i].doc.file)
				if rel == "" {
					rel = hits[i].doc.file
				}
				top = append(top, topHit{Name: hits[i].doc.name, File: rel, Score: hits[i].score})
			}
			t.Logf("  docs=%d embed_total=%s avg=%.2fms query_embed=%s top1=%s(%.3f)",
				len(docs), stats.total, stats.avgMS, qElapsed, top[0].Name, top[0].Score)

			results = append(results, runResult{
				Model:        m.name,
				Corpus:       c.name,
				DocCount:     len(docs),
				EmbedTotal:   stats.total.String(),
				EmbedTotalMS: float64(stats.total.Nanoseconds()) / 1e6,
				EmbedAvgMS:   stats.avgMS,
				QueryMS:      float64(qElapsed.Nanoseconds()) / 1e6,
				TopHits:      top,
			})
		}
	}

	if err := writeResults(results); err != nil {
		t.Fatalf("write results: %v", err)
	}
	t.Logf("wrote %d run rows", len(results))
}

// ───────────────────────────────────────────────────────────────────────
// Discovery
// ───────────────────────────────────────────────────────────────────────

// modelRoot is the directory the bench scans for installed models.
func modelRoot() string {
	if p := os.Getenv("EMBED_BENCH_MODEL_DIR"); p != "" {
		return p
	}
	home := os.Getenv("VESKA_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".veska")
	}
	return filepath.Join(home, "static-model")
}

// discoverModels returns the BenchModels subset that's actually on disk
// (tokenizer.json + model.safetensors both present). Missing models drop
// out with a logged warning so the bench is runnable with whatever's
// installed.
func discoverModels(t *testing.T) []modelEntry {
	t.Helper()
	root := modelRoot()
	var out []modelEntry
	for _, name := range BenchModels {
		dir := filepath.Join(root, name)
		tok := filepath.Join(dir, "tokenizer.json")
		st := filepath.Join(dir, "model.safetensors")
		if !fileNonEmpty(tok) || !fileNonEmpty(st) {
			t.Logf("WARN: model %s not installed at %s — skip (run scripts/install-bench-models.sh)", name, dir)
			continue
		}
		out = append(out, modelEntry{name: name, dir: dir})
	}
	return out
}

// discoverCorpora resolves each named corpus to a root directory.
// "veska" is always this repo; the rest live under out/repos/<name>/.
// Missing corpora are logged and skipped — run scripts/fetch-corpora.sh
// to fetch them.
func discoverCorpora(t *testing.T) []corpusEntry {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(file)
	repoRoot := filepath.Clean(filepath.Join(pkgDir, "..", "..", ".."))
	fetchedRoot := filepath.Join(pkgDir, "out", "repos")

	var out []corpusEntry
	for _, name := range BenchCorpora {
		var root string
		switch name {
		case "veska":
			// Scope to internal/ — covers application + infrastructure
			// + domain. Excludes cmd/ (which is small and CLI-shaped)
			// and tools/ (eval harnesses, not "production" content).
			root = filepath.Join(repoRoot, "internal")
		default:
			root = filepath.Join(fetchedRoot, name)
		}
		if !dirExists(root) {
			t.Logf("WARN: corpus %s not present at %s — skip (run scripts/fetch-corpora.sh)", name, root)
			continue
		}
		out = append(out, corpusEntry{name: name, root: root})
	}
	return out
}

// ───────────────────────────────────────────────────────────────────────
// Embedding
// ───────────────────────────────────────────────────────────────────────

type embedStats struct {
	avgMS float64
	total time.Duration
}

// embedCorpus walks every .go file under root (skipping _test.go and
// vendor/), parses each with the production Go parser, and embeds each
// declaration's name+raw_content as a single document. Capped at
// maxDocs to bound runtime when iterating over many (model × corpus)
// combinations.
func embedCorpus(t *testing.T, p *model2vec.Provider, root string, maxDocs int) ([]doc, embedStats) {
	t.Helper()
	parser := treesitter.NewGoParser()
	var docs []doc
	start := time.Now()
	var totalEmbedNS int64
	var nEmbeds int

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate per-entry walk errors
		}
		if d.IsDir() {
			// Skip vendor/, .git/, and any node_modules-style noise.
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || name == "out" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if len(docs) >= maxDocs {
			return filepath.SkipAll
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		result, err := parser.ParseFile(context.Background(), "bench", path, src)
		if err != nil || result == nil {
			return nil
		}
		for _, n := range result.Nodes {
			if len(docs) >= maxDocs {
				return filepath.SkipAll
			}
			if n.RawContent == nil || *n.RawContent == "" {
				continue
			}
			text := n.Name + "\n" + *n.RawContent
			t0 := time.Now()
			v, err := p.Embed(context.Background(), text)
			totalEmbedNS += time.Since(t0).Nanoseconds()
			nEmbeds++
			if err != nil {
				continue
			}
			docs = append(docs, doc{name: n.Name, file: path, vec: v})
		}
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		t.Logf("walk %s: %v", root, walkErr)
	}
	stats := embedStats{total: time.Since(start)}
	if nEmbeds > 0 {
		stats.avgMS = float64(totalEmbedNS) / float64(nEmbeds) / 1e6
	}
	return docs, stats
}

// ───────────────────────────────────────────────────────────────────────
// Ranking
// ───────────────────────────────────────────────────────────────────────

type hit struct {
	doc   doc
	score float32
}

// rank computes a dot product between the query vector and each
// document's vector, returning hits sorted by score descending.
// model2vec vectors are L2-normalised, so dot product ≡ cosine.
func rank(q []float32, docs []doc) []hit {
	hits := make([]hit, 0, len(docs))
	for _, d := range docs {
		hits = append(hits, hit{d, dot(q, d.vec)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	return hits
}

func dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
		return 0
	}
	return s
}

// ───────────────────────────────────────────────────────────────────────
// Output
// ───────────────────────────────────────────────────────────────────────

func writeResults(rows []runResult) error {
	out := os.Getenv("EMBED_BENCH_OUT")
	if out == "" {
		_, file, _, _ := runtime.Caller(0)
		out = filepath.Join(filepath.Dir(file), "out", "results.json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	r := benchResults{
		Phase:       "0k5h.2",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Runs:        rows,
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o644)
}

// ───────────────────────────────────────────────────────────────────────
// Helpers
// ───────────────────────────────────────────────────────────────────────

func fileNonEmpty(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func modelNames(ms []modelEntry) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.name
	}
	return out
}

func corpusNames(cs []corpusEntry) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.name
	}
	return out
}

// silence unused-import warnings when the file is built alone.
var _ = fmt.Sprintf
