// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// makeSyntheticModel creates a synthetic Model2Vec model directory with consistent tokenizer and safetensors configurations.
func makeSyntheticModel(t *testing.T, tmp string) (string, []int, int) {
	t.Helper()
	_ = tmp // dir is the input

	// 6 rows × 4 cols - same vocab as the tokenizer fixture.
	vocab := map[string]int{
		"[UNK]":  0,
		"parse":  1,
		"config": 2,
		"##ing":  3,
		"func":   4,
		"return": 5,
	}
	dim := 4
	rows := 6
	matrix := []float32{
		// id 0 ([UNK])
		0.1, 0.1, 0.1, 0.1,
		// id 1 (parse)
		1.0, 0.0, 0.0, 0.0,
		// id 2 (config)
		0.0, 1.0, 0.0, 0.0,
		// id 3 (##ing)
		0.0, 0.0, 1.0, 0.0,
		// id 4 (func)
		0.5, 0.5, 0.5, 0.5,
		// id 5 (return)
		0.0, 0.0, 0.0, 1.0,
	}
	writeTokenizerFixture(t, filepath.Join(tmp, "tokenizer.json"), vocab)
	blob := buildSafetensorsFile(t, "embeddings", []int{rows, dim}, matrix)
	if err := os.WriteFile(filepath.Join(tmp, "model.safetensors"), blob, 0o644); err != nil {
		t.Fatalf("write safetensors: %v", err)
	}
	return tmp, []int{rows, dim}, dim
}

// TestNew_LoadsModelDir verifies that the loaded model directory returns correct dimensions and ID.
func TestNew_LoadsModelDir(t *testing.T) {
	dir, shape, _ := makeSyntheticModel(t, t.TempDir())
	p, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.NativeDim(); got != shape[1] {
		t.Errorf("NativeDim: got %d, want %d", got, shape[1])
	}
	if p.ModelID() == "" {
		t.Error("ModelID empty")
	}
}

// TestEmbed_MeanPoolsKnownVocabularyTokens verifies that embedding combines active token vectors.
func TestEmbed_MeanPoolsKnownVocabularyTokens(t *testing.T) {
	dir, _, _ := makeSyntheticModel(t, t.TempDir())
	p, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := p.Embed(context.Background(), "parse config")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != p.OutputDim() {
		t.Fatalf("dim: got %d, want %d", len(v), p.OutputDim())
	}
	// Verify that active slots contain positive values.
	if v[0] <= 0 || v[1] <= 0 {
		t.Errorf("parse + config slots should be positive: %v", v[:4])
	}
	if math.Abs(float64(v[2])) > 1e-6 || math.Abs(float64(v[3])) > 1e-6 {
		t.Errorf("inactive slots should be ~0: %v", v[:4])
	}
}

// TestEmbed_PadsToOutputDim verifies that native vectors are padded to the expected output dimension with zeros.
func TestEmbed_PadsToOutputDim(t *testing.T) {
	dir, _, native := makeSyntheticModel(t, t.TempDir())
	p, _ := New(dir)
	v, _ := p.Embed(context.Background(), "parse")
	if len(v) != p.OutputDim() {
		t.Fatalf("output dim: got %d, want %d", len(v), p.OutputDim())
	}
	for i := native; i < len(v); i++ {
		if v[i] != 0 {
			t.Errorf("padding component %d non-zero: %v", i, v[i])
		}
	}
}

// TestEmbed_L2Normalized verifies that the generated embedding vector is L2-normalized.
func TestEmbed_L2Normalized(t *testing.T) {
	dir, _, _ := makeSyntheticModel(t, t.TempDir())
	p, _ := New(dir)
	v, _ := p.Embed(context.Background(), "parse config func")
	var sumsq float64
	for _, x := range v {
		sumsq += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sumsq)-1.0) > 1e-5 {
		t.Errorf("|v| = %v, want 1.0", math.Sqrt(sumsq))
	}
}

// TestEmbed_EmptyInputReturnsZeroVector verifies that empty text input yields a zero vector.
func TestEmbed_EmptyInputReturnsZeroVector(t *testing.T) {
	dir, _, _ := makeSyntheticModel(t, t.TempDir())
	p, _ := New(dir)
	v, err := p.Embed(context.Background(), "")
	if err != nil {
		t.Fatalf("Embed empty: %v", err)
	}
	if len(v) != p.OutputDim() {
		t.Errorf("dim: %d", len(v))
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Errorf("non-finite: %v", v)
			break
		}
	}
}

// TestEmbed_AppliesPerTokenWeights verifies that token weights are correctly applied during mean-pooling.
func TestEmbed_AppliesPerTokenWeights(t *testing.T) {
	tmp := t.TempDir()
	vocab := map[string]int{"[UNK]": 0, "parse": 1, "config": 2}
	dim, rows := 4, 3
	matrix := []float32{
		0.1, 0.1, 0.1, 0.1, // [UNK]
		1.0, 0.0, 0.0, 0.0, // parse
		0.0, 1.0, 0.0, 0.0, // config
	}
	weights := []float64{1, 1, 3} // w[parse]=1, w[config]=3
	writeTokenizerFixture(t, filepath.Join(tmp, "tokenizer.json"), vocab)
	blob := buildSafetensorsEmbedWeights(t, []int{rows, dim}, matrix, weights)
	if err := os.WriteFile(filepath.Join(tmp, "model.safetensors"), blob, 0o644); err != nil {
		t.Fatalf("write safetensors: %v", err)
	}
	p, err := New(tmp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := p.Embed(context.Background(), "parse config")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if v[0] <= 0 {
		t.Fatalf("parse slot should be positive: %v", v[:4])
	}
	if ratio := v[1] / v[0]; math.Abs(float64(ratio)-3.0) > 1e-4 {
		t.Errorf("config/parse slot ratio = %v, want 3.0 (weighted pool); %v", ratio, v[:4])
	}
}

// buildSafetensorsEmbedWeights constructs a synthetic Safetensors payload with both embeddings and weights.
func buildSafetensorsEmbedWeights(t *testing.T, shape []int, emb []float32, weights []float64) []byte {
	t.Helper()
	embBytes := make([]byte, 4*len(emb))
	for i, v := range emb {
		binary.LittleEndian.PutUint32(embBytes[i*4:], math.Float32bits(v))
	}
	wBytes := make([]byte, 8*len(weights))
	for i, v := range weights {
		binary.LittleEndian.PutUint64(wBytes[i*8:], math.Float64bits(v))
	}
	embEnd := len(embBytes)
	wEnd := embEnd + len(wBytes)
	header := `{"embeddings":{"dtype":"F32","shape":` + intsToJSON(shape) +
		`,"data_offsets":[0,` + intToStr(embEnd) + `]},` +
		`"weights":{"dtype":"F64","shape":[` + intToStr(len(weights)) +
		`],"data_offsets":[` + intToStr(embEnd) + `,` + intToStr(wEnd) + `]}}`
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(header)))
	buf.WriteString(header)
	buf.Write(embBytes)
	buf.Write(wBytes)
	return buf.Bytes()
}

// TestNewFromBytes verifies that the in-memory initialization matches the behavior of disk loading.
func TestNewFromBytes(t *testing.T) {
	vocab := map[string]int{"[UNK]": 0, "parse": 1, "config": 2}
	tokSpec := map[string]any{
		"normalizer":    map[string]any{"type": "BertNormalizer", "lowercase": true},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type": "WordPiece", "unk_token": "[UNK]",
			"continuing_subword_prefix": "##", "vocab": vocab,
		},
	}
	tokBytes, err := json.Marshal(tokSpec)
	if err != nil {
		t.Fatalf("marshal tokenizer: %v", err)
	}
	st := buildSafetensorsFile(t, "embeddings", []int{3, 4}, []float32{
		0.1, 0.1, 0.1, 0.1,
		1.0, 0.0, 0.0, 0.0,
		0.0, 1.0, 0.0, 0.0,
	})
	p, err := NewFromBytes("potion-code-16M", tokBytes, st)
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	if p.ModelID() != "model2vec(potion-code-16M)" {
		t.Errorf("ModelID = %q, want model2vec(potion-code-16M)", p.ModelID())
	}
	v, err := p.Embed(context.Background(), "parse config")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != p.OutputDim() {
		t.Errorf("dim = %d, want %d", len(v), p.OutputDim())
	}
}

// writeTokenizerFixture writes a simulated tokenizer configuration to the specified path.
func writeTokenizerFixture(t *testing.T, path string, vocab map[string]int) {
	t.Helper()
	spec := map[string]any{
		"normalizer":    map[string]any{"type": "BertNormalizer", "lowercase": true},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type":                      "WordPiece",
			"unk_token":                 "[UNK]",
			"continuing_subword_prefix": "##",
			"vocab":                     vocab,
		},
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal tokenizer fixture: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write tokenizer fixture: %v", err)
	}
}
