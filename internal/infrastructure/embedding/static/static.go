// Package static is a CPU-only, in-process EmbeddingProvider that
// requires zero external services - no Ollama, no Python sidecar, no
// network, no model file.
// Algorithm (v2, FastText-style subword hashing):
//  1. Tokenise into camelCase / snake_case / non-alphanumeric subwords.
//  2. For each token, derive character n-grams (3.6) plus the token
//     itself, with explicit boundary markers ('<' / '>') so prefix and
//     suffix morphology contributes - FastText's trick.
//  3. Hash each n-gram to a Dim-length float32 vector via SHA-256
//     expansion mapped into [-1, 1].
//  4. Sum all n-gram vectors across all tokens; divide by count
//     (mean-pool); L2-normalise.
//
// This is NOT a faithful Model2Vec / potion-code-16M port - that work
// is filed as and requires a HuggingFace tokenizer +
// safetensors loader. The v2 subword scheme captures the property the
// v1 per-token hash could not: identifiers sharing morphology
// ("parseConfig" vs "configParser") land closer in vector space than
// unrelated ones. That's the recall floor a static embedder needs to
// be useful at all on code.
// Production-quality embeddings still come from Ollama. The static
// embedder is the lowest rung of the pick-one embedder ladder
// (static-v2 < model2vec < Ollama; see elect.go) - one embedder owns
// the index at a time, since vectors from different models live in
// incompatible spaces. Static unblocks first-run setup and zero-dep
// CPU runs; it doesn't replace Ollama for serious work.
package static

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// ModelID is the embedding-cache key reported by Provider.ModelID.
// Bumping the suffix invalidates every static-embedded vector - done
// here moving from v1 (per-token hash) to v2 (subword n-gram hash).
// The "veska-" prefix prevents collision with any external model name.
const ModelID = "veska-static-v2"

// ngramMin and ngramMax bound the character-n-gram window applied per
// token. 3.6 matches FastText's defaults; on code identifiers
// (typically 4.16 chars) this captures enough overlap to give
// "parseConfig" / "configParser" a meaningful cosine without
// exploding the per-token n-gram count.
const (
	ngramMin = 3
	ngramMax = 6
)

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

// Embed returns a deterministic L2-normalised vector for text.
// Empty / whitespace-only input still returns a finite normalised
// vector (the empty-string hash) so callers don't have to special-case
// a NaN result.
func (*Provider) Embed(_ context.Context, text string) ([]float32, error) {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return l2Normalize(hashVector("")), nil
	}
	acc := make([]float32, Dim)
	var count int
	for _, tok := range tokens {
		for _, ng := range subwordNgrams(tok) {
			v := hashVector(ng)
			for i := range Dim {
				acc[i] += v[i]
			}
			count++
		}
	}
	if count == 0 {
		// Defensive: every non-empty token yields at least one n-gram
		// (the whole token), but a degenerate input would otherwise
		// land at all-zero → l2Normalize handles that branch too.
		return l2Normalize(acc), nil
	}
	inv := 1.0 / float32(count)
	for i := range Dim {
		acc[i] *= inv
	}
	return l2Normalize(acc), nil
}

// subwordNgrams returns the FastText-style subword set for tok: every
// character n-gram with length in [ngramMin, ngramMax] over the token
// surrounded by '<' and '>' boundary markers, plus the whole token as
// a standalone n-gram (so single-letter shared tokens still match).
// The boundary markers let "<par" only match identifiers that start
// with "par", giving the model a notion of prefix vs suffix.
func subwordNgrams(tok string) []string {
	if tok == "" {
		return nil
	}
	bounded := "<" + tok + ">"
	bounds := []byte(bounded)
	// Code identifiers are ASCII in the overwhelming majority of cases;
	// byte-indexed n-grams are correct for them. Non-ASCII identifiers
	// (rare) still get the whole-token fallback below.
	var out []string
	for n := ngramMin; n <= ngramMax; n++ {
		if len(bounds) < n {
			break
		}
		for i := 0; i+n <= len(bounds); i++ {
			out = append(out, string(bounds[i:i+n]))
		}
	}
	out = append(out, tok)
	return out
}

// tokenize splits s on non-alphanumeric runes AND on camelCase /
// PascalCase boundaries (Foo|Bar, HTTP|Server) so subword n-grams
// reflect the natural morphology of code identifiers. Output is
// lowercased so case never participates in the cosine.
func tokenize(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			// Boundary forms (mirrors the rerank splitIdentifier):
			//   prev lower / digit, this upper: camelCase (Foo|Bar)
			//   prev upper, next lower (in an acronym run): HTTP|Server
			if unicode.IsLower(prev) || unicode.IsDigit(prev) ||
				(unicode.IsUpper(prev) && next != 0 && unicode.IsLower(next)) {
				flush()
			}
		}
		cur.WriteRune(unicode.ToLower(r))
	}
	flush()
	return out
}

// hashVector derives a Dim-length pseudo-vector from key by expanding
// SHA-256 across Dim/8 hash rounds, packing 8 float32 components per
// round. Each component is mapped into [-1, 1] via
// (uint32 → float64 / max → 2x - 1). Cheap, deterministic, dimension
// stable. Used for both per-token and per-n-gram hashing - same math,
// just different inputs.
func hashVector(key string) []float32 {
	out := make([]float32, Dim)
	const wordsPerRound = 8 // 8 uint32s per SHA-256
	rounds := (Dim + wordsPerRound - 1) / wordsPerRound
	for r := range rounds {
		h := sha256.New()
		h.Write([]byte{byte(r), byte(r >> 8)})
		h.Write([]byte(key))
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
