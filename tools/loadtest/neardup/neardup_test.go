//go:build eval

// Near-duplicate threshold-calibration harness (solov2-md3n). Embeds the
// curated corpus through one or more real providers, scores every pair through
// the production memvec path (Score = 1/(1+L2^2)), and reports the per-tier
// score distributions so DefaultNearThreshold can be set with data rather than
// guessed.
//
// Run:
//
//	go test -tags "eval embed_model" -run TestNearDupThreshold -v \
//	    ./tools/loadtest/neardup/
//
// embed_model compiles in the potion-code-16M weights so model2vec runs with
// no service and no download. nomic-embed-text is measured additionally when
// an Ollama daemon is reachable (VESKA_OLLAMA_URL, default localhost:11434);
// it is skipped cleanly otherwise.
//
// The threshold lives in an embedder-specific score space (vectors from
// different models are not comparable), so the harness reports EACH embedder
// separately and the chosen DefaultNearThreshold names the model it was
// calibrated on.
package neardup

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// embedder is the minimal contract the harness needs from a provider; both
// model2vec.Provider and ollama.Provider satisfy it.
type embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	ModelID() string
}

// tierStats summarises one tier's score distribution.
type tierStats struct {
	Tier   string  `json:"tier"`
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	P10    float64 `json:"p10"`
	Median float64 `json:"median"`
	P90    float64 `json:"p90"`
	Max    float64 `json:"max"`
}

// embedderResult is the per-embedder calibration outcome.
type embedderResult struct {
	Embedder       string      `json:"embedder"`
	Tiers          []tierStats `json:"tiers"`
	Separated      bool        `json:"separated"`       // min(neardup) > max(related)
	RecommendedMin float64     `json:"recommended_min"` // midpoint of the gap when separated
	NearDupMin     float64     `json:"near_dup_min"`    // weakest near-dup score
	RelatedMax     float64     `json:"related_max"`     // strongest "related" score
	RelatedP90     float64     `json:"related_p90"`     // robust upper edge of "related"
}

func TestNearDupThreshold(t *testing.T) {
	texts := flatten()
	t.Logf("corpus: %d texts across %d bases", len(texts), len(corpus))

	embedders := availableEmbedders(t)
	if len(embedders) == 0 {
		t.Skip("no embedders available")
	}

	results := make([]embedderResult, 0, len(embedders))
	for name, e := range embedders {
		res := calibrate(t, name, e, texts)
		results = append(results, res)
		reportEmbedder(t, res)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Embedder < results[j].Embedder })
	if err := writeJSON("neardup_threshold_results.json", results); err != nil {
		t.Fatalf("write results: %v", err)
	}
}

// availableEmbedders returns the providers to calibrate. model2vec is the
// primary target (code-specific potion-code-16M, no service — representative
// of veska's default-quality path); nomic is included for cross-reference and
// gate-3 continuity when Ollama is up.
func availableEmbedders(t *testing.T) map[string]embedder {
	out := map[string]embedder{}

	if p, ok := model2vec.Embedded(); ok {
		out["model2vec/"+p.ModelID()] = p
	} else {
		t.Log("model2vec embedded weights unavailable (build without embed_model?) — skipping")
	}

	base := os.Getenv("VESKA_OLLAMA_URL")
	if base == "" {
		base = "http://localhost:11434"
	}
	if p, err := ollama.New("nomic-embed-text", ollama.WithBaseURL(base), ollama.WithTimeout(15*time.Second)); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, perr := p.Embed(ctx, "package main"); perr == nil {
			out["ollama/nomic-embed-text"] = p
		} else {
			t.Logf("ollama nomic-embed-text unreachable (%v) — skipping", perr)
		}
		cancel()
	}
	return out
}

// calibrate embeds every text, scores every pair through memvec, and folds the
// scores into per-tier distributions.
func calibrate(t *testing.T, name string, e embedder, texts []labeledText) embedderResult {
	ctx := context.Background()
	store, err := vector.NewVectorStorage(vector.BackendMemory, t.TempDir())
	if err != nil {
		t.Fatalf("%s: NewVectorStorage: %v", name, err)
	}

	rows := make([]domain.EmbeddingRow, len(texts))
	vecByID := make(map[string][]float32, len(texts))
	for i, lt := range texts {
		raw, err := e.Embed(ctx, lt.text)
		if err != nil {
			t.Fatalf("%s: embed %s: %v", name, lt.id, err)
		}
		v := l2normalize(raw) // prod's embedder pipeline L2-normalises before storage
		vecByID[lt.id] = v
		rows[i] = domain.EmbeddingRow{NodeID: lt.id, ContentHash: "h-" + lt.id, ModelID: e.ModelID(), Vector: v}
	}
	if err := store.UpsertEmbeddings(ctx, "neardup", "main", rows); err != nil {
		t.Fatalf("%s: UpsertEmbeddings: %v", name, err)
	}

	byTier := map[tier][]float64{}
	for i := range texts {
		hits, err := store.Search(ctx, "neardup", "main", vecByID[texts[i].id], len(texts), domain.VectorFilter{})
		if err != nil {
			t.Fatalf("%s: search %s: %v", name, texts[i].id, err)
		}
		score := make(map[string]float64, len(hits))
		for _, h := range hits {
			score[h.NodeID] = float64(h.Score)
		}
		for j := i + 1; j < len(texts); j++ {
			tr := classify(texts[i], texts[j])
			byTier[tr] = append(byTier[tr], score[texts[j].id])
		}
	}
	return summarise(name, byTier)
}

// summarise turns the per-tier score samples into an embedderResult with
// distribution stats and a separation verdict.
func summarise(name string, byTier map[tier][]float64) embedderResult {
	res := embedderResult{Embedder: name}
	for _, tr := range []tier{tierNearDup, tierRelated, tierUnrelated} {
		res.Tiers = append(res.Tiers, statsFor(tr.String(), byTier[tr]))
	}
	near := append([]float64(nil), byTier[tierNearDup]...)
	rel := append([]float64(nil), byTier[tierRelated]...)
	sort.Float64s(near)
	sort.Float64s(rel)
	if len(near) > 0 {
		res.NearDupMin = near[0]
	}
	if len(rel) > 0 {
		res.RelatedMax = rel[len(rel)-1]
		res.RelatedP90 = percentile(rel, 0.90)
	}
	res.Separated = len(near) > 0 && len(rel) > 0 && res.NearDupMin > res.RelatedMax
	if res.Separated {
		res.RecommendedMin = (res.NearDupMin + res.RelatedMax) / 2
	}
	return res
}

func statsFor(name string, xs []float64) tierStats {
	s := tierStats{Tier: name, Count: len(xs)}
	if len(xs) == 0 {
		return s
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	s.Min = sorted[0]
	s.Max = sorted[len(sorted)-1]
	s.P10 = percentile(sorted, 0.10)
	s.Median = percentile(sorted, 0.50)
	s.P90 = percentile(sorted, 0.90)
	return s
}

// percentile returns the p-quantile of an already-sorted slice (nearest-rank).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func reportEmbedder(t *testing.T, r embedderResult) {
	t.Logf("── %s ──", r.Embedder)
	for _, s := range r.Tiers {
		t.Logf("  %-9s n=%-3d min=%.3f p10=%.3f med=%.3f p90=%.3f max=%.3f",
			s.Tier, s.Count, s.Min, s.P10, s.Median, s.P90, s.Max)
	}
	if r.Separated {
		t.Logf("  SEPARATED: near-dup min %.3f > related max %.3f → recommend threshold ≈ %.3f",
			r.NearDupMin, r.RelatedMax, r.RecommendedMin)
	} else {
		t.Logf("  OVERLAP: near-dup min %.3f <= related max %.3f (related p90 %.3f) → threshold-based near-dup is noisy here",
			r.NearDupMin, r.RelatedMax, r.RelatedP90)
	}
}

func l2normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1.0 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
