//go:build eval

// Condensation axis for the embed-models bench (solov2-oo4q.2).
//
// When EMBED_BENCH_CONDENSE=on, the bench computes a SECOND embedding
// per document using extractive condensation (top-K most-central pieces
// of the raw content, joined and re-embedded). Both vectors are stored
// per doc and both recall maps appear in results.json + the published
// table, so the lift (condensed_R@10 - raw_R@10) is directly visible
// per (model × corpus) cell.
//
// Knobs:
//   EMBED_BENCH_CONDENSE         on|off (default off)
//   EMBED_BENCH_CONDENSE_K       int (default 5) — top-K pieces kept
//   EMBED_BENCH_CONDENSE_MIN_LEN int (default 500) — skip docs shorter
//                                than this many bytes (short symbols
//                                don't need condensing and condensation
//                                of <5-line bodies is a no-op anyway)
//
// Cost note: condensation requires len(pieces) extra embeds per doc
// (for centrality scoring) plus one more for the joined result. With
// model2vec that's microseconds per piece — adds ~3 min to a full
// model2vec sweep. With Ollama it would be ruinous (~hours) — keep
// EMBED_BENCH_CONDENSE off when the Ollama subset is enabled, or
// expect long runtimes.

package embed_models

import (
	"context"
	"os"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/embedder/condense"
)

// condenseConfig holds the bench-time condensation knobs.
type condenseConfig struct {
	enabled bool
	k       int
	minLen  int
}

func loadCondenseConfig() condenseConfig {
	return condenseConfig{
		enabled: os.Getenv("EMBED_BENCH_CONDENSE") == "on",
		k:       envInt("EMBED_BENCH_CONDENSE_K", 5),
		minLen:  envInt("EMBED_BENCH_CONDENSE_MIN_LEN", 500),
	}
}

// embedderAdapter wraps the bench's narrow Embedder interface to
// satisfy ports.EmbeddingProvider (which condense.Condense requires).
// The ModelID() stub is unused by the condenser — it only calls Embed.
type embedderAdapter struct{ inner Embedder }

func (a embedderAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.inner.Embed(ctx, text)
}
func (a embedderAdapter) ModelID() string { return "bench" }

// splitPieces splits raw content into condensation-input pieces. For
// both code and prose we split on newlines and drop blank lines. The
// per-piece granularity is good enough for centrality scoring at this
// stage; sentence-level prose splitting can be a follow-up if per-line
// centrality plateaus during oo4q.3 production wiring.
func splitPieces(raw string) []string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// condenseInput returns the condensed embed-text for a doc, or the raw
// embed-text unchanged if the doc is shorter than minLen or has fewer
// than 2 pieces (centrality is undefined). The boolean reports whether
// condensation was actually applied — counted into the per-run
// CondenseAppliedCount for diagnostics.
//
// The name is ALWAYS prepended to the result so the embed input shape
// is identical to the raw path (`name + "\n" + body`). Without the
// name prepend, short helper symbols would land as vectors derived
// only from body content, which would skew recall against the raw
// baseline.
func condenseInput(ctx context.Context, p Embedder, cfg condenseConfig, name, raw string) (string, bool, error) {
	if !cfg.enabled || len(raw) < cfg.minLen {
		return name + "\n" + raw, false, nil
	}
	pieces := splitPieces(raw)
	if len(pieces) < 2 {
		return name + "\n" + raw, false, nil
	}
	kept, err := condense.Condense(ctx, embedderAdapter{p}, pieces, cfg.k)
	if err != nil {
		return "", false, err
	}
	return name + "\n" + strings.Join(kept, "\n"), true, nil
}
