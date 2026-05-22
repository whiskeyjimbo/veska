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

// makeSyntheticModel builds a self-consistent vocab + tokenizer.json +
// safetensors model.bin in tmp dir; returns the directory. Each vocab
// id maps to its own row in the embedding matrix so the test can
// reason about exact arithmetic.
func makeSyntheticModel(t *testing.T, tmp string) (string, []int, int) {
	t.Helper()
	_ = tmp // dir is the input

	// 6 rows × 4 cols — same vocab as the tokenizer fixture.
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

// TestNew_LoadsModelDir loads the synthetic fixture and verifies the
// Provider reports the expected dim + vocab size.
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

// TestEmbed_MeanPoolsKnownVocabularyTokens: "parse config" tokenises
// to [parse(1), config(2)]; embedding is mean-pool of rows 1 + 2.
// Pre-pad pre-normalize values: (0.5, 0.5, 0, 0). After L2-normalize:
// component magnitudes are equal in the parse/config slots and zero
// elsewhere; checking the relative ordering is enough.
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
	// The native 4-dim portion of v should have non-zero parse/config
	// slots and zero ##ing / return slots.
	if v[0] <= 0 || v[1] <= 0 {
		t.Errorf("parse + config slots should be positive: %v", v[:4])
	}
	if math.Abs(float64(v[2])) > 1e-6 || math.Abs(float64(v[3])) > 1e-6 {
		t.Errorf("inactive slots should be ~0: %v", v[:4])
	}
}

// TestEmbed_PadsToOutputDim: native dim is 4 but we project to
// OutputDim() (768) for index compatibility — zero-pad the tail so
// the cosine in the native subspace is preserved.
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

// TestEmbed_L2Normalized pins the unit-magnitude invariant. The
// vector storage's cosine math assumes |v|=1 — without this every
// model2vec hit would have its score systematically biased by the
// raw mean-pool magnitude.
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

// TestEmbed_EmptyInputReturnsZeroVector: empty input produces no
// tokens — the embedder must return a finite, dim-correct vector.
// Returning all-zeros is preferable to NaN (cosine of zero is 0,
// which is fine).
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

// TestEmbed_AppliesPerTokenWeights: when a "weights" tensor is present
// the pool is a weighted mean, matching the reference model2vec library
// (potion-* models ship these weights; a plain mean lands ~0.91 cosine
// off the reference, which would invalidate any recall comparison).
// vocab: parse=1, config=2 with rows e1=[1,0,0,0], e2=[0,1,0,0] and
// weights w1=1, w2=3 ⇒ weighted mean of "parse config" is
// [0.25,0.75,0,0], so after L2-normalize slot[1] == 3*slot[0]. A plain
// (unweighted) mean would make the two slots equal.
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

// buildSafetensorsEmbedWeights builds a two-tensor safetensors blob:
// "embeddings" (F32, shape rows×dim) + "weights" (F64, length rows),
// the layout real potion-* models ship.
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

// writeTokenizerFixture writes a synthetic tokenizer.json to path
// using the same BertNormalizer+BertPreTokenizer+WordPiece pipeline
// the real model uses.
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
