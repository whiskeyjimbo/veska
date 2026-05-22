// Package static is a CPU-only, in-process EmbeddingProvider that
// requires zero external services — no Ollama, no Python sidecar, no
// network. Search works on a fresh machine without setup
// (solov2-soc).
//
// It is NOT a faithful Model2Vec port: the design memo locks in a
// real static-embedding model as the long-term goal (see memory
// "soc-design"), but this MVP ships a deterministic hash-based
// embedder that produces stable, L2-normalised vectors at the same
// 768-dim shape as nomic-embed-text so the existing vector index and
// search service drop in unchanged.
//
// Quality trade-off: hash-derived token vectors carry no semantic
// information, so two semantically related symbols (ParseConfig and
// configParser) won't cluster the way they would under a real static
// model. The reranker (solov2-2sf), hybrid retrieval (solov2-2su),
// and chunk index (solov2-jyt) layers above compensate by surfacing
// lexically-similar matches even when the vector cosine is noisy.
// Setting up Ollama remains the recommended path for production
// quality; the static embedder unblocks first-run evaluation.
package static

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// ModelID is the embedding-cache key reported by Provider.ModelID().
// Bumping the suffix invalidates every static-embedded vector — do so
// only when the math changes (different hash, different dim, different
// pooling). The "veska-" prefix prevents collision with any external
// model name.
const ModelID = "veska-static-v1"

// Dim is the output dimensionality. 768 matches nomic-embed-text so
// the static embedder is a drop-in replacement at the vector-storage
// layer without a schema migration.
const Dim = 768

// Provider is the static EmbeddingProvider adapter.
type Provider struct{}

// New constructs a Provider. The signature mirrors ollama.New so the
// daemon's composition root can swap the two without rework.
func New() (*Provider, error) {
	return &Provider{}, nil
}

// ModelID returns the stable identifier of the embedding model in use.
func (*Provider) ModelID() string { return ModelID }

// Embed returns a deterministic L2-normalised vector for text:
//
//  1. tokenise — lowercase, split on non-alphanumeric;
//  2. per-token hash — SHA-256 expanded into Dim float32 components
//     scaled into [-1, 1];
//  3. mean-pool the token vectors;
//  4. L2-normalise.
//
// Empty / whitespace-only input still returns a finite normalised
// vector (the empty-string hash) so callers don't have to special-case
// a NaN result.
func (*Provider) Embed(_ context.Context, text string) ([]float32, error) {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return l2Normalize(tokenVector("")), nil
	}
	acc := make([]float32, Dim)
	for _, tok := range tokens {
		tv := tokenVector(tok)
		for i := range Dim {
			acc[i] += tv[i]
		}
	}
	inv := 1.0 / float32(len(tokens))
	for i := range Dim {
		acc[i] *= inv
	}
	return l2Normalize(acc), nil
}

// tokenize lower-cases and splits on non-alphanumeric runes. Cheap and
// language-agnostic — sufficient for the hash-pooling scheme where the
// vector quality bottleneck is the per-token hash, not the tokeniser.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// tokenVector derives a Dim-length pseudo-vector from token by
// expanding SHA-256 of the token across Dim/8 hash rounds, packing
// 8 float32 components per round. Each component is mapped into
// [-1, 1] via (uint32 → float64 / max → 2x - 1). Cheap, deterministic,
// dimension-stable.
func tokenVector(token string) []float32 {
	out := make([]float32, Dim)
	const wordsPerRound = 8 // 8 uint32s per SHA-256
	rounds := (Dim + wordsPerRound - 1) / wordsPerRound
	for r := range rounds {
		h := sha256.New()
		h.Write([]byte{byte(r), byte(r >> 8)})
		h.Write([]byte(token))
		sum := h.Sum(nil)
		for w := range wordsPerRound {
			idx := r*wordsPerRound + w
			if idx >= Dim {
				break
			}
			u := binary.BigEndian.Uint32(sum[w*4:])
			// Map [0, 2^32) → [-1, 1).
			out[idx] = float32(float64(u)/float64(1<<32)*2 - 1)
		}
	}
	return out
}

// l2Normalize divides the vector by its L2 magnitude in place and
// returns it. A zero-magnitude input is left untouched and an
// identity-ish vector (1/sqrt(Dim) per component) is returned so
// downstream cosine math stays finite.
func l2Normalize(v []float32) []float32 {
	var sumsq float64
	for _, x := range v {
		sumsq += float64(x) * float64(x)
	}
	if sumsq == 0 {
		fill := float32(1.0 / math.Sqrt(float64(len(v))))
		for i := range v {
			v[i] = fill
		}
		return v
	}
	mag := float32(math.Sqrt(sumsq))
	for i := range v {
		v[i] /= mag
	}
	return v
}
