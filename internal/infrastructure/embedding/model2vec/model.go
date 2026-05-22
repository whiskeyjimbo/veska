// Package model2vec is a pure-Go EmbeddingProvider that runs Model2Vec
// inference against a pre-distilled static embedding model
// (potion-code-16M is the design target; see solov2-vn0).
//
// Inference is trivial — Model2Vec's whole point is that there is no
// transformer at query time:
//
//  1. Tokenise text with the model's HuggingFace WordPiece tokenizer.
//  2. Look up each token id's row in the embedding matrix.
//  3. Mean-pool the per-token vectors.
//  4. L2-normalise.
//
// The native model dimension (e.g. 256) is zero-padded to OutputDim
// (768) so vectors slot into the existing nomic-shaped index without
// a schema change. The padded tail carries zero magnitude, so within
// the index cosine ranking against other model2vec vectors is
// preserved — but model2vec vectors are NOT comparable to Ollama
// vectors that live in a different 768-dim subspace. The composite
// EmbeddingProvider's ModelID-as-cache-key invariant already accepts
// this trade-off: a configuration swap invalidates the cache.
//
// Model files (tokenizer.json + model.safetensors) live in
// <VeskaHome>/static-model/<model-name>/. The download.go path can
// fetch + sha-verify them from a HuggingFace base URL, but auto-
// download on daemon start is gated on a future config flag — today
// users opt in by manually placing the files:
//
//	mkdir -p ~/.veska/static-model/potion-code-16M
//	curl -L https://huggingface.co/minishlab/potion-code-16M/resolve/main/tokenizer.json    -o ~/.veska/static-model/potion-code-16M/tokenizer.json
//	curl -L https://huggingface.co/minishlab/potion-code-16M/resolve/main/model.safetensors -o ~/.veska/static-model/potion-code-16M/model.safetensors
//
// The daemon's composite chain (cmd/veska-daemon/wire.go) calls
// TryLoad; ErrModelNotPresent triggers the hash-static fallback.
package model2vec

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// OutputDim is the index-compatible vector dimension. Vectors from
// this provider are zero-padded from NativeDim() up to OutputDim so
// they fit the existing 768-dim sqlite-vec / usearch schema.
const OutputDim = 768

// embeddingsTensorName is the conventional key safetensors uses for
// the matrix Model2Vec models ship. If a downstream model uses a
// different name, surface a clear error rather than silently embed
// against random data.
const embeddingsTensorName = "embeddings"

// Provider is the model2vec EmbeddingProvider adapter.
type Provider struct {
	tk        *tokenizer
	matrix    []float32 // flat row-major
	vocabSize int
	nativeDim int
	modelID   string
}

// New loads a model directory containing tokenizer.json +
// model.safetensors and returns a ready-to-embed Provider. The
// directory's basename is used as the model identifier — bumping the
// distilled model swaps the ID and invalidates the embedding cache.
func New(modelDir string) (*Provider, error) {
	tkBytes, err := os.ReadFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("model2vec: read tokenizer.json: %w", err)
	}
	tk, err := newTokenizer(tkBytes)
	if err != nil {
		return nil, err
	}
	stf, err := os.Open(filepath.Join(modelDir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("model2vec: open model.safetensors: %w", err)
	}
	defer stf.Close()
	tensors, err := readSafetensors(stf)
	if err != nil {
		return nil, err
	}
	emb, ok := tensors[embeddingsTensorName]
	if !ok {
		return nil, fmt.Errorf("model2vec: safetensors missing %q tensor", embeddingsTensorName)
	}
	if len(emb.Shape) != 2 {
		return nil, fmt.Errorf("model2vec: embeddings shape must be 2-D, got %v", emb.Shape)
	}
	if emb.Shape[1] > OutputDim {
		return nil, fmt.Errorf("model2vec: native dim %d exceeds OutputDim %d", emb.Shape[1], OutputDim)
	}
	return &Provider{
		tk:        tk,
		matrix:    emb.Data,
		vocabSize: emb.Shape[0],
		nativeDim: emb.Shape[1],
		modelID:   "model2vec(" + filepath.Base(modelDir) + ")",
	}, nil
}

// ModelID returns the stable cache key.
func (p *Provider) ModelID() string { return p.modelID }

// NativeDim is the model's actual embedding dimension before padding.
// Exposed for tests + observability.
func (p *Provider) NativeDim() int { return p.nativeDim }

// OutputDim is the padded dimension every Embed call returns.
func (*Provider) OutputDim() int { return OutputDim }

// Embed tokenises, looks up rows, mean-pools, normalises, and pads.
// An empty input (or one with no usable tokens) returns a zero vector
// rather than an error — callers cannot distinguish "no content" from
// "no signal" anyway, and propagating an error here would break the
// composite's clean delegation contract.
func (p *Provider) Embed(_ context.Context, text string) ([]float32, error) {
	ids := p.tk.encode(text)
	if len(ids) == 0 {
		return make([]float32, OutputDim), nil
	}

	acc := make([]float32, p.nativeDim)
	var count int
	for _, id := range ids {
		if id < 0 || id >= p.vocabSize {
			continue // safety — shouldn't happen with a self-consistent model
		}
		row := p.matrix[id*p.nativeDim : (id+1)*p.nativeDim]
		for i := range p.nativeDim {
			acc[i] += row[i]
		}
		count++
	}
	if count == 0 {
		return make([]float32, OutputDim), nil
	}
	inv := 1.0 / float32(count)
	for i := range p.nativeDim {
		acc[i] *= inv
	}

	// L2-normalise in the native subspace; padding stays zero.
	var sumsq float64
	for _, x := range acc {
		sumsq += float64(x) * float64(x)
	}
	out := make([]float32, OutputDim)
	if sumsq == 0 {
		return out, nil
	}
	mag := float32(math.Sqrt(sumsq))
	for i := range p.nativeDim {
		out[i] = acc[i] / mag
	}
	return out, nil
}
