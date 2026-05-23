//go:build eval

// Package embed_models benchmarks embedding model variants — model2vec
// in-process providers and (in later phases) Ollama models — over real
// codebase corpora, so hi5's defaults are data-backed and end users get
// a published comparison table (solov2-0k5h).
//
// 0k5h.1 — minimum vertical slice: load ONE model, embed a single corpus
// (a configurable subtree of this repo by default), run ONE hardcoded
// query, print top-K. Proves the loop; later phases iterate over many
// (model × corpus × query) combinations and compute recall@k.
//
// Run with: make eval-embed-models
//   Env knobs:
//     EMBED_BENCH_MODEL_DIR  — model2vec model directory
//                              (default: $VESKA_HOME/static-model/potion-code-16M)
//     EMBED_BENCH_CORPUS     — directory to walk for .go files
//                              (default: <repo>/internal/core/domain)
//     EMBED_BENCH_QUERY      — query string to run
//                              (default: "load configuration from file")
//     EMBED_BENCH_TOPK       — number of top results to print (default 10)
package embed_models

import (
	"context"
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

// doc is one embedded corpus document. The bench's brute-force k-NN runs
// over a slice of these — phase 0k5h.1 doesn't need a real vector index.
type doc struct {
	name string
	file string
	vec  []float32
}

func TestEmbedModelsBenchmark(t *testing.T) {
	provider, modelDir := loadModel(t)
	corpus := corpusRoot()
	query := envOr("EMBED_BENCH_QUERY", "load configuration from file")
	topK := envInt("EMBED_BENCH_TOPK", 10)

	t.Logf("model=%s", modelDir)
	t.Logf("corpus=%s", corpus)
	t.Logf("query=%q topK=%d", query, topK)

	docs, embedStats := embedCorpus(t, provider, corpus)
	if len(docs) == 0 {
		t.Fatalf("no documents embedded from %s", corpus)
	}
	t.Logf("embedded %d documents (avg=%.2fms total=%s)",
		len(docs), embedStats.avgMS, embedStats.total)

	qStart := time.Now()
	qvec, err := provider.Embed(context.Background(), query)
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	qElapsed := time.Since(qStart)
	t.Logf("embed query: %s", qElapsed)

	hits := rank(qvec, docs)
	if topK > len(hits) {
		topK = len(hits)
	}
	fmt.Printf("\nQuery: %q\nTop %d:\n", query, topK)
	for i := 0; i < topK; i++ {
		rel, err := filepath.Rel(corpus, hits[i].doc.file)
		if err != nil {
			rel = hits[i].doc.file
		}
		fmt.Printf("  %2d. %.4f  %-40s  (%s)\n", i+1, hits[i].score, hits[i].doc.name, rel)
	}
}

// loadModel reads EMBED_BENCH_MODEL_DIR or falls back to the default
// model2vec install location under VESKA_HOME.
func loadModel(t *testing.T) (*model2vec.Provider, string) {
	t.Helper()
	modelDir := os.Getenv("EMBED_BENCH_MODEL_DIR")
	if modelDir == "" {
		home := os.Getenv("VESKA_HOME")
		if home == "" {
			home = filepath.Join(os.Getenv("HOME"), ".veska")
		}
		modelDir = filepath.Join(home, "static-model", "potion-code-16M")
	}
	p, err := model2vec.New(modelDir)
	if err != nil {
		t.Fatalf("load model %s: %v", modelDir, err)
	}
	return p, modelDir
}

// corpusRoot returns the directory the bench walks for .go files.
// The default is this repo's internal/core/domain — small (~few hundred
// symbols) so 0k5h.1 stays quick to iterate on.
func corpusRoot() string {
	if p := os.Getenv("EMBED_BENCH_CORPUS"); p != "" {
		return p
	}
	// Resolve relative to this test file so it works regardless of $PWD.
	_, file, _, _ := runtime.Caller(0)
	// tools/loadtest/embed_models/embed_models_test.go → repo root via three ..
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "internal", "core", "domain"))
}

type embedStats struct {
	avgMS float64
	total time.Duration
}

// embedCorpus walks every .go file under root (skipping _test.go), parses
// each with the production Go parser, and embeds each declaration's
// name + raw_content as a single document.
func embedCorpus(t *testing.T, p *model2vec.Provider, root string) ([]doc, embedStats) {
	t.Helper()
	parser := treesitter.NewGoParser()
	var docs []doc
	start := time.Now()
	var totalEmbedNS int64
	var nEmbeds int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
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
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	stats := embedStats{total: time.Since(start)}
	if nEmbeds > 0 {
		stats.avgMS = float64(totalEmbedNS) / float64(nEmbeds) / 1e6
	}
	return docs, stats
}

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
	// Guard against NaN/Inf creeping in from broken vectors.
	if math.IsNaN(float64(s)) || math.IsInf(float64(s), 0) {
		return 0
	}
	return s
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
