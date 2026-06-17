// Package model2vec implements a pure-Go EmbeddingProvider executing Model2Vec inference against static embedding models.
package model2vec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// OutputDim defines the uniform output vector dimension. Native vectors are zero-padded to this size.
const OutputDim = 768

// embeddingsTensorName is the tensor name for the embedding matrix in Safetensors format.
const embeddingsTensorName = "embeddings"

// weightsTensorName is the tensor name for optional per-token pooling weights.
const weightsTensorName = "weights"

// Provider is the Model2Vec embedding provider.
type Provider struct {
	tk        *tokenizer
	matrix    []float32 // Embedding matrix data stored flat in row-major order.
	weights   []float32 // Optional per-token weights for weighted mean-pooling.
	vocabSize int
	nativeDim int
	modelID   string
}

// New loads model and tokenizer files from the specified directory.
func New(modelDir string) (*Provider, error) {
	tkBytes, err := os.ReadFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("model2vec: read tokenizer.json: %w", err)
	}
	stf, err := os.Open(filepath.Join(modelDir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("model2vec: open model.safetensors: %w", err)
	}
	defer stf.Close()
	return newFromParts(filepath.Base(modelDir), tkBytes, stf)
}

// NewFromBytes initializes a Provider directly from tokenizer and Safetensors byte payloads.
func NewFromBytes(name string, tokenizerJSON, safetensors []byte) (*Provider, error) {
	return newFromParts(name, tokenizerJSON, bytes.NewReader(safetensors))
}

// newFromParts constructs the Model2Vec provider from configuration parts.
func newFromParts(name string, tkBytes []byte, safetensors io.Reader) (*Provider, error) {
	tk, err := newTokenizer(tkBytes)
	if err != nil {
		return nil, err
	}
	tensors, err := readSafetensors(safetensors)
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
	vocabSize := emb.Shape[0]

	// Verify that the optional per-token weights match the vocabulary size if present.
	var weights []float32
	if w, ok := tensors[weightsTensorName]; ok {
		if len(w.Data) != vocabSize {
			return nil, fmt.Errorf("model2vec: weights length %d != vocab %d", len(w.Data), vocabSize)
		}
		weights = w.Data
	}

	return &Provider{
		tk:        tk,
		matrix:    emb.Data,
		weights:   weights,
		vocabSize: vocabSize,
		nativeDim: emb.Shape[1],
		modelID:   "model2vec(" + name + ")",
	}, nil
}

// ModelID returns the model identifier.
func (p *Provider) ModelID() string { return p.modelID }

// NativeDim returns the model's native vector dimensions.
func (p *Provider) NativeDim() int { return p.nativeDim }

// OutputDim returns the output dimensions.
func (*Provider) OutputDim() int { return OutputDim }

// Embed runs tokenizer tokenization, embedding lookup, pooling, and L2-normalization on the input text.
func (p *Provider) Embed(_ context.Context, text string) ([]float32, error) {
	ids := p.tk.encode(text)
	if len(ids) == 0 {
		return make([]float32, OutputDim), nil
	}

	acc := make([]float32, p.nativeDim)
	var wsum float32
	for _, id := range ids {
		if id < 0 || id >= p.vocabSize {
			continue // Ignore out-of-bounds token IDs.
		}
		w := float32(1)
		if p.weights != nil {
			w = p.weights[id]
		}
		row := p.matrix[id*p.nativeDim : (id+1)*p.nativeDim]
		for i := range p.nativeDim {
			acc[i] += w * row[i]
		}
		wsum += w
	}
	if wsum == 0 {
		return make([]float32, OutputDim), nil
	}
	inv := 1.0 / wsum
	for i := range p.nativeDim {
		acc[i] *= inv
	}

	// Perform L2-normalization on the native vector workspace.
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
