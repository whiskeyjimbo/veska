//go:build eval

// Package embed_models benchmarks embedding model variants — model2vec
// in-process providers and (in later phases) Ollama models — over real
// codebase corpora, so hi5's defaults are data-backed and end users get
// a published comparison table .
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
//
//	Env knobs:
//	  EMBED_BENCH_MODEL_DIR  — override the model search path (default:
//	                           $VESKA_HOME/static-model)
//	  EMBED_BENCH_QUERY      — query string used for the printed top-K
//	                           sanity check (default: "load config")
//	  EMBED_BENCH_TOPK       — number of top results to print (default 10)
//	  EMBED_BENCH_MAX_DOCS   — cap docs per corpus to bound runtime
//	                           during iteration (default 5000)
//	  EMBED_BENCH_OUT        — path to write results JSON
//	                           (default: tools/loadtest/embed_models/out/results.json)
package embed_models

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// BenchModels lists the model2vec variants the bench targets.
// Order is footprint-ascending so output is easier to scan.
// Note: minishlab's lineup tops out at 32M for the English-monolingual
// variants — the 128M tier exists only as potion-multilingual-128M
// (different use case, deferred to a follow-up).
var BenchModels = []string{
	"potion-base-2M",
	"potion-base-4M",
	"potion-base-8M",
	"potion-code-16M",
	"potion-retrieval-32M",
	"potion-base-32M",
}

// BenchOllamaModels lists the Ollama embedders the -full variant
// of the bench targets (0k5h.5). Enabled by EMBED_BENCH_INCLUDE_OLLAMA=1
// and only when an Ollama service is reachable at $VESKA_OLLAMA_URL.
var BenchOllamaModels = []string{
	"nomic-embed-text",
	"bge-m3",
	"snowflake-arctic-embed",
	"mxbai-embed-large",
}

// Embedder is the minimum surface every bench-target provider satisfies
// — both model2vec.Provider and ollama.Provider implement this exact
// signature. Keeps the corpus walkers and the recall metric independent
// of the concrete provider type.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BenchCorpora lists the corpus names the bench targets. veska is
// always present (this repo); the rest live under out/repos/<name>/
// after scripts/fetch-corpora.sh runs. Prose corpora (suffix "-docs"
// or "wikipedia-*") walk .md files via embedProseCorpus.
var BenchCorpora = []string{
	// Code corpora
	"veska", "cobra", "pflag", "testify", "gin",
	// Prose corpora — dense-technical
	"veska-docs", "cobra-docs",
	// Prose corpora — genuine natural prose (Wikipedia software-topic articles)
	"wikipedia-tech",
}

// doc is one embedded corpus document. vecCondensed is populated only
// when EMBED_BENCH_CONDENSE=on; otherwise nil and the second recall
// pass is skipped. The two vectors share name/file because they
// describe the same source node — only the embed input differs.
type doc struct {
	name         string
	file         string
	vec          []float32
	vecCondensed []float32
}

// modelEntry pairs a model name with its on-disk dir, used for
// iteration. Resolved at test start; missing models drop out of the
// list with a logged warning.
type modelEntry struct {
	name string
	dir  string
}

// corpusEntry pairs a corpus name with its root directory and its kind
// ("code" walks .go files via tree-sitter; "prose" walks .md files via
// the section splitter in prose.go).
type corpusEntry struct {
	name string
	root string
	kind string
}

// runResult captures per-(model × corpus) bench data — what gets
// written to results.json and consumed by 0k5h.6's table generator.
type runResult struct {
	Model        string                  `json:"model"`
	ModelType    string                  `json:"model_type"` // "model2vec" (in-process) or "ollama" (network)
	Corpus       string                  `json:"corpus"`
	DocCount     int                     `json:"doc_count"`
	EmbedTotal   string                  `json:"embed_total"`    // human-readable duration
	EmbedTotalMS float64                 `json:"embed_total_ms"` // machine-readable
	EmbedAvgMS   float64                 `json:"embed_avg_ms"`
	QueryMS      float64                 `json:"query_ms"` // query embed time
	TopHits      []topHit                `json:"top_hits"` // sanity-check top-K for the printed query
	Recall       map[string]RecallScores `json:"recall"`   // gt-source → scores (headline / doc / test-name)
	// Condensation axis (solov2-oo4q.2). Populated only when
	// EMBED_BENCH_CONDENSE=on. CondensedRecall is the recall produced
	// when ranking docs by their condensed vector; raw Recall is
	// preserved so the lift is row-comparable.
	CondensedRecall      map[string]RecallScores `json:"condensed_recall,omitempty"`
	CondenseK            int                     `json:"condense_k,omitempty"`
	CondenseMinLen       int                     `json:"condense_min_len,omitempty"`
	CondenseAppliedCount int                     `json:"condense_applied_count,omitempty"`
}

type topHit struct {
	Name  string  `json:"name"`
	File  string  `json:"file"`
	Score float32 `json:"score"`
}

// benchResults is the on-disk JSON shape — a phase number + every
// per-run row. Later phases (0k5h.3 recall, 0k5h.6 table) read it.
type benchResults struct {
	Phase       string      `json:"phase"`
	GeneratedAt string      `json:"generated_at"`
	Runs        []runResult `json:"runs"`
}

func TestEmbedModelsBenchmark(t *testing.T) {
	models := discoverModels(t)
	corpora := discoverCorpora(t)
	if len(corpora) == 0 {
		t.Fatalf("no corpora available (veska self-corpus should always resolve)")
	}

	query := envOr("EMBED_BENCH_QUERY", "load config")
	topK := envInt("EMBED_BENCH_TOPK", 10)
	maxDocs := envInt("EMBED_BENCH_MAX_DOCS", 5000)
	ollamaMaxDocs := envInt("EMBED_BENCH_OLLAMA_MAX_DOCS", 500)

	t.Logf("model2vec found: %d (%v)", len(models), modelNames(models))
	t.Logf("corpora found: %d (%v)", len(corpora), corpusNames(corpora))
	t.Logf("query=%q topK=%d max_docs=%d", query, topK, maxDocs)

	condenseCfg := loadCondenseConfig()
	if condenseCfg.enabled {
		t.Logf("condensation: ON (k=%d min_len=%d) — each doc gets a second condensed-vec embed; recall computed for both",
			condenseCfg.k, condenseCfg.minLen)
	}

	includeOllama := os.Getenv("EMBED_BENCH_INCLUDE_OLLAMA") == "1"
	var ollamaModels []string
	if includeOllama {
		if reachable, url := ollamaReachable(); reachable {
			ollamaModels = BenchOllamaModels
			t.Logf("ollama reachable at %s — including %d models (max_docs=%d)", url, len(ollamaModels), ollamaMaxDocs)
		} else {
			t.Logf("WARN: EMBED_BENCH_INCLUDE_OLLAMA=1 but ollama unreachable at %s — skipping ollama subset", url)
		}
	}

	if len(models) == 0 && len(ollamaModels) == 0 {
		t.Fatalf("no embedders available (model2vec set empty AND ollama subset disabled or unreachable)")
	}

	var results []runResult
	runOne := func(modelName, modelType string, provider Embedder, c corpusEntry, mxDocs int) {
		t.Logf("--- run: model=%s [%s] corpus=%s (%s) ---", modelName, modelType, c.name, c.kind)
		var (
			docs      []doc
			stats     embedStats
			condCount int
		)
		switch c.kind {
		case "prose":
			docs, stats, condCount = embedProseCorpus(provider, c.root, mxDocs, condenseCfg)
		default:
			docs, stats, condCount = embedCorpus(t, provider, c.root, mxDocs, condenseCfg)
		}
		if len(docs) == 0 {
			t.Logf("  no docs from %s — skip", c.root)
			return
		}
		qStart := time.Now()
		qvec, err := provider.Embed(context.Background(), query)
		if err != nil {
			t.Logf("  embed query failed: %v — skip", err)
			return
		}
		qElapsed := time.Since(qStart)
		hits := rank(qvec, docs)
		k := topK
		if k > len(hits) {
			k = len(hits)
		}
		top := make([]topHit, 0, k)
		for i := 0; i < k; i++ {
			rel, _ := filepath.Rel(c.root, hits[i].doc.file)
			if rel == "" {
				rel = hits[i].doc.file
			}
			top = append(top, topHit{Name: hits[i].doc.name, File: rel, Score: hits[i].score})
		}
		t.Logf("  docs=%d embed_total=%s avg=%.2fms query_embed=%s top1=%s(%.3f)",
			len(docs), stats.total, stats.avgMS, qElapsed, top[0].Name, top[0].Score)

		gtSources := CollectGroundTruth(c.name, c.root, fixturesDir(), c.kind)
		recallByGT := make(map[string]RecallScores, len(gtSources))
		for _, gt := range gtSources {
			if len(gt.Pairs) == 0 {
				recallByGT[gt.Name] = RecallScores{}
				continue
			}
			scores := ComputeRecall(provider, gt.Pairs, docs)
			recallByGT[gt.Name] = scores
			t.Logf("  recall[%s] n=%d @10=%.3f mrr=%.3f  fair_n=%d fair_@10=%.3f fair_mrr=%.3f  miss=%d not_in_corpus=%d",
				gt.Name, scores.N, scores.At10, scores.MRR,
				scores.FairN, scores.FairAt10, scores.FairMRR,
				scores.Miss, scores.NotInCorpus)
		}

		// Condensation pass: build a parallel docs slice whose .vec is
		// the condensed vector (where one was computed) and re-run
		// recall. Docs without a condensed vec are kept with the raw
		// vec — that's the natural "below the minLen gate" behavior:
		// short docs participate in the index unchanged, just not
		// condensed. The query vec is unchanged (queries are short,
		// condensation makes no sense for them).
		var condensedRecall map[string]RecallScores
		if condenseCfg.enabled {
			condDocs := make([]doc, len(docs))
			copy(condDocs, docs)
			for i := range condDocs {
				if condDocs[i].vecCondensed != nil {
					condDocs[i].vec = condDocs[i].vecCondensed
				}
			}
			condensedRecall = make(map[string]RecallScores, len(gtSources))
			for _, gt := range gtSources {
				if len(gt.Pairs) == 0 {
					condensedRecall[gt.Name] = RecallScores{}
					continue
				}
				scores := ComputeRecall(provider, gt.Pairs, condDocs)
				condensedRecall[gt.Name] = scores
				raw := recallByGT[gt.Name]
				lift := scores.FairAt10 - raw.FairAt10
				t.Logf("  CONDENSED recall[%s] @10=%.3f fair_@10=%.3f  LIFT=%+.3f  applied=%d/%d",
					gt.Name, scores.At10, scores.FairAt10, lift, condCount, len(docs))
			}
		}

		row := runResult{
			Model:        modelName,
			ModelType:    modelType,
			Corpus:       c.name,
			DocCount:     len(docs),
			EmbedTotal:   stats.total.String(),
			EmbedTotalMS: float64(stats.total.Nanoseconds()) / 1e6,
			EmbedAvgMS:   stats.avgMS,
			QueryMS:      float64(qElapsed.Nanoseconds()) / 1e6,
			TopHits:      top,
			Recall:       recallByGT,
		}
		if condenseCfg.enabled {
			row.CondensedRecall = condensedRecall
			row.CondenseK = condenseCfg.k
			row.CondenseMinLen = condenseCfg.minLen
			row.CondenseAppliedCount = condCount
		}
		results = append(results, row)
		// Incremental persistence: write results + markdown after EVERY
		// (model × corpus) run, not just at end of test. A long bench
		// that times out mid-Ollama then keeps every completed row
		// instead of losing them all. Negligible cost (file size is
		// tens of KB; writes are infrequent relative to the per-embed
		// loop). Errors are logged but non-fatal.
		if err := writeResults(results); err != nil {
			t.Logf("WARN: incremental write results: %v", err)
		}
		if err := writeMarkdownTable(results); err != nil {
			t.Logf("WARN: incremental write table: %v", err)
		}
	}

	for _, m := range models {
		provider, err := model2vec.New(m.dir)
		if err != nil {
			t.Logf("WARN: load %s: %v — skipping", m.name, err)
			continue
		}
		for _, c := range corpora {
			runOne(m.name, "model2vec", provider, c, maxDocs)
		}
	}

	for _, om := range ollamaModels {
		provider, err := ollama.New(om, ollama.WithBaseURL(ollamaURL()))
		if err != nil {
			t.Logf("WARN: ollama.New %s: %v — skipping", om, err)
			continue
		}
		// Per-model probe: ollama.New does not validate the model is
		// pulled — that fails on first Embed. Probe with a tiny query
		// so a model the user hasn't `ollama pull`'d skips cleanly
		// instead of failing every corpus run.
		probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, perr := provider.Embed(probeCtx, "probe")
		cancel()
		if perr != nil {
			t.Logf("WARN: ollama %s probe failed: %v — skipping (run 'ollama pull %s' to include)", om, perr, om)
			continue
		}
		for _, c := range corpora {
			runOne(om, "ollama", provider, c, ollamaMaxDocs)
		}
	}

	if err := writeResults(results); err != nil {
		t.Fatalf("write results: %v", err)
	}
	t.Logf("wrote %d run rows", len(results))
	if err := writeMarkdownTable(results); err != nil {
		t.Logf("WARN: write markdown table: %v", err)
	} else {
		t.Logf("refreshed docs/operations/embedder-benchmarks.md")
	}
}

// ───────────────────────────────────────────────────────────────────────
// Discovery
// ───────────────────────────────────────────────────────────────────────

// ollamaURL returns the bench's Ollama base URL. Defaults to the
// production VESKA_OLLAMA_URL or localhost.
func ollamaURL() string {
	if u := os.Getenv("VESKA_OLLAMA_URL"); u != "" {
		return u
	}
	return "http://localhost:11434"
}

// ollamaReachable probes the Ollama service with a short timeout. Used
// to skip the Ollama subset cleanly when the service isn't running,
// rather than failing the whole bench. Returns the URL we tried so the
// caller can log it on either branch.
func ollamaReachable() (bool, string) {
	u := ollamaURL()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(u + "/api/tags")
	if err != nil {
		return false, u
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500, u
}

// fixturesDir resolves the bench's hand-curated ground-truth directory.
// Override with EMBED_BENCH_FIXTURES; default is fixtures/ under the
// package (committed). Every *.jsonl file inside is loaded and merged
// — see CollectGroundTruth.
func fixturesDir() string {
	if p := os.Getenv("EMBED_BENCH_FIXTURES"); p != "" {
		return p
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "fixtures")
}

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
		var root, kind string
		switch name {
		case "veska":
			// Scope to internal/ — covers application + infrastructure
			// + domain. Excludes cmd/ (which is small and CLI-shaped)
			// and tools/ (eval harnesses, not "production" content).
			root = filepath.Join(repoRoot, "internal")
			kind = "code"
		case "veska-docs":
			root = filepath.Join(repoRoot, "docs")
			kind = "prose"
		case "cobra-docs":
			root = filepath.Join(fetchedRoot, "cobra")
			kind = "prose"
		case "wikipedia-tech":
			// Pulled by scripts/fetch-wikipedia-corpus.sh from Wikipedia's
			// MediaWiki API. CC-BY-SA 3.0 content; out/ is gitignored.
			_, file, _, _ := runtime.Caller(0)
			root = filepath.Join(filepath.Dir(file), "out", "wikipedia-tech")
			kind = "prose"
		default:
			root = filepath.Join(fetchedRoot, name)
			kind = "code"
		}
		if !dirExists(root) {
			t.Logf("WARN: corpus %s not present at %s — skip (run scripts/fetch-corpora.sh)", name, root)
			continue
		}
		out = append(out, corpusEntry{name: name, root: root, kind: kind})
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
//
// When cfg.enabled, ALSO computes a condensed-input embedding per doc
// and stores it in doc.vecCondensed. Returns the count of docs that
// were actually condensed (vs skipped by minLen gate).
func embedCorpus(t *testing.T, p Embedder, root string, maxDocs int, cfg condenseConfig) ([]doc, embedStats, int) {
	t.Helper()
	parser := treesitter.NewGoParser()
	var docs []doc
	var condApplied int
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
			d := doc{name: n.Name, file: path, vec: v}
			if cfg.enabled {
				cText, applied, cerr := condenseInput(context.Background(), p, cfg, n.Name, *n.RawContent)
				if cerr == nil {
					cv, cerr := p.Embed(context.Background(), cText)
					if cerr == nil {
						d.vecCondensed = cv
						if applied {
							condApplied++
						}
					}
				}
			}
			docs = append(docs, d)
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
	return docs, stats, condApplied
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
		Phase:       "0k5h.6",
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
