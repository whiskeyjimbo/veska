package model2vec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTryLoad_NoFilesReturnsErrModelNotPresent: a fresh install has
// no <VeskaHome>/static-model/* dir. The daemon relies on the
// errors.Is(err, ErrModelNotPresent) gate to decide whether to skip
// model2vec and use only the hash-static fallback — without this
// sentinel, the daemon would either crash on boot or silently
// surface a more confusing "open: no such file" error.
func TestTryLoad_NoFilesReturnsErrModelNotPresent(t *testing.T) {
	home := t.TempDir()
	_, err := TryLoad(home, "potion-code-16M")
	if !errors.Is(err, ErrModelNotPresent) {
		t.Fatalf("expected ErrModelNotPresent, got %v", err)
	}
}

// TestTryLoad_SuccessWhenFilesPresent: when both tokenizer.json and
// model.safetensors live under <VeskaHome>/static-model/<name>/,
// TryLoad returns a working Provider.
func TestTryLoad_SuccessWhenFilesPresent(t *testing.T) {
	home := t.TempDir()
	modelDir := filepath.Join(home, "static-model", "fake-model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTokenizerFixture(t, filepath.Join(modelDir, "tokenizer.json"), map[string]int{
		"[UNK]": 0, "hello": 1, "world": 2,
	})
	blob := buildSafetensorsFile(t, "embeddings", []int{3, 4}, []float32{
		0.1, 0.1, 0.1, 0.1,
		1, 0, 0, 0,
		0, 1, 0, 0,
	})
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := TryLoad(home, "fake-model")
	if err != nil {
		t.Fatalf("TryLoad: %v", err)
	}
	if p.NativeDim() != 4 {
		t.Errorf("NativeDim: %d", p.NativeDim())
	}
}

// TestTryLoad_HalfPresentReturnsErrModelNotPresent: when only one of
// the two required files is on disk, treat the model as not-installed
// rather than half-installed. A partial download is exactly the
// "missing" state from the consumer's perspective.
func TestTryLoad_HalfPresentReturnsErrModelNotPresent(t *testing.T) {
	home := t.TempDir()
	modelDir := filepath.Join(home, "static-model", "fake-model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "tokenizer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := TryLoad(home, "fake-model")
	if !errors.Is(err, ErrModelNotPresent) {
		t.Fatalf("expected ErrModelNotPresent for half-installed model, got %v", err)
	}
}
