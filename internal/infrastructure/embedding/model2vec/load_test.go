// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTryLoad_NoFilesReturnsErrModelNotPresent verifies that TryLoad returns ErrModelNotPresent if model files are absent.
func TestTryLoad_NoFilesReturnsErrModelNotPresent(t *testing.T) {
	home := t.TempDir()
	_, err := TryLoad(home, "potion-code-16M")
	if !errors.Is(err, ErrModelNotPresent) {
		t.Fatalf("expected ErrModelNotPresent, got %v", err)
	}
}

// TestTryLoad_SuccessWhenFilesPresent verifies that TryLoad successfully initializes the provider when both model files are present.
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

// TestTryLoad_HalfPresentReturnsErrModelNotPresent verifies that having only one model file returns ErrModelNotPresent.
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
