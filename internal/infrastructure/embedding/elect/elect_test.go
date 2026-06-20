// SPDX-License-Identifier: AGPL-3.0-only

package elect

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
)

// writeMinimalModel2Vec creates a minimal valid Model2Vec model directory on disk for local testing.
func writeMinimalModel2Vec(t *testing.T, veskaHome, name string) {
	t.Helper()
	dir := filepath.Join(veskaHome, "static-model", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir model dir: %v", err)
	}
	tok := map[string]any{
		"normalizer":    map[string]any{"type": "BertNormalizer", "lowercase": true},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model": map[string]any{
			"type": "WordPiece", "unk_token": "[UNK]",
			"continuing_subword_prefix": "##",
			"vocab":                     map[string]int{"[UNK]": 0, "foo": 1},
		},
	}
	b, _ := json.Marshal(tok)
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), b, 0o644); err != nil {
		t.Fatalf("write tokenizer: %v", err)
	}
	// safetensors defines one F32 "embeddings" tensor with shape [2,4].
	emb := []float32{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8}
	data := make([]byte, 4*len(emb))
	for i, v := range emb {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(v))
	}
	header := `{"embeddings":{"dtype":"F32","shape":[2,4],"data_offsets":[0,32]}}`
	var buf []byte
	hdrLen := make([]byte, 8)
	binary.LittleEndian.PutUint64(hdrLen, uint64(len(header)))
	buf = append(buf, hdrLen...)
	buf = append(buf, header...)
	buf = append(buf, data...)
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), buf, 0o644); err != nil {
		t.Fatalf("write safetensors: %v", err)
	}
}

func staticModelID(t *testing.T) string {
	t.Helper()
	p, err := embedstatic.New()
	if err != nil {
		t.Fatalf("static.New: %v", err)
	}
	return p.ModelID()
}

func TestElect_AutoPicksModel2VecWhenInstalled(t *testing.T) {
	home := t.TempDir()
	writeMinimalModel2Vec(t, home, "potion-code-16M")
	res, err := Elect(Config{VeskaHome: home, Override: OverrideAuto})
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if !strings.HasPrefix(res.Name, "model2vec(") {
		t.Errorf("auto should pick model2vec when installed, got %q", res.Name)
	}
	if got := markerContents(t, home); got != res.Name {
		t.Errorf("marker = %q, want %q", got, res.Name)
	}
}

func TestElect_AutoFallsBackToStatic(t *testing.T) {
	home := t.TempDir()
	res, err := Elect(Config{VeskaHome: home, Override: OverrideAuto})
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if res.Name != staticModelID(t) {
		t.Errorf("auto without model2vec should pick static %q, got %q", staticModelID(t), res.Name)
	}
}

func TestElect_OverrideModel2VecMissingErrors(t *testing.T) {
	home := t.TempDir()
	_, err := Elect(Config{VeskaHome: home, Override: OverrideModel2Vec})
	if err == nil || !strings.Contains(err.Error(), "veska install model2vec") {
		t.Errorf("expected install-hint error, got %v", err)
	}
}

func TestElect_OverrideOllama(t *testing.T) {
	home := t.TempDir()
	res, err := Elect(Config{VeskaHome: home, Override: OverrideOllama, EmbedModel: "nomic-embed-text", OllamaURL: "http://localhost:11434"})
	if err != nil {
		t.Fatalf("Elect: %v", err)
	}
	if res.Name != "nomic-embed-text" {
		t.Errorf("ollama name = %q, want nomic-embed-text", res.Name)
	}
}

func TestElect_UnknownOverrideErrors(t *testing.T) {
	_, err := Elect(Config{VeskaHome: t.TempDir(), Override: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown VESKA_EMBEDDER") {
		t.Errorf("expected unknown-override error, got %v", err)
	}
}

func TestElect_MarkerSwitchDetection(t *testing.T) {
	home := t.TempDir()
	// First boot simulates an initial election with a fresh marker.
	r1, err := Elect(Config{VeskaHome: home, Override: OverrideStatic})
	if err != nil {
		t.Fatalf("Elect 1: %v", err)
	}
	if r1.Previous != "" || r1.SwitchedModel {
		t.Errorf("fresh election: Previous=%q SwitchedModel=%v, want empty/false", r1.Previous, r1.SwitchedModel)
	}
	// Second boot simulates checking without altering the elected model.
	r2, err := Elect(Config{VeskaHome: home, Override: OverrideStatic})
	if err != nil {
		t.Fatalf("Elect 2: %v", err)
	}
	if r2.SwitchedModel {
		t.Errorf("same embedder should not report a switch")
	}
	// Third boot simulates switching the embedder to trigger switch detection.
	r3, err := Elect(Config{VeskaHome: home, Override: OverrideOllama, EmbedModel: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("Elect 3: %v", err)
	}
	if !r3.SwitchedModel || r3.Previous != r1.Name {
		t.Errorf("switch: SwitchedModel=%v Previous=%q (want true, %q)", r3.SwitchedModel, r3.Previous, r1.Name)
	}
}

func markerContents(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, markerFile))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return strings.TrimSpace(string(b))
}
