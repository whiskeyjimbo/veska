// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureModel_FreshDownload verifies that ensureModel downloads all files from the server into an empty directory.
func TestEnsureModel_FreshDownload(t *testing.T) {
	tokJSON := []byte(`{"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0}}}`)
	stBytes := []byte("safetensors-bytes")
	srv := newFakeHFServer(map[string][]byte{
		"/tokenizer.json":    tokJSON,
		"/model.safetensors": stBytes,
	})
	defer srv.Close()

	dir := t.TempDir()
	spec := ModelSpec{
		BaseURL: srv.URL,
		Files: []FileSpec{
			{Name: "tokenizer.json", SHA256: sha256Hex(tokJSON)},
			{Name: "model.safetensors", SHA256: sha256Hex(stBytes)},
		},
	}
	if err := ensureModel(context.Background(), dir, spec); err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	for _, f := range spec.Files {
		got, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		if sha256Hex(got) != f.SHA256 {
			t.Errorf("%s: sha mismatch", f.Name)
		}
	}
}

// TestInstall_WritesToModelDir verifies that the Install function resolves and populates the model directory.
func TestInstall_WritesToModelDir(t *testing.T) {
	tokJSON := []byte(`{"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0}}}`)
	stBytes := []byte("safetensors-bytes")
	srv := newFakeHFServer(map[string][]byte{
		"/tokenizer.json":    tokJSON,
		"/model.safetensors": stBytes,
	})
	defer srv.Close()

	veskaHome := t.TempDir()
	spec := ModelSpec{
		BaseURL: srv.URL,
		Files: []FileSpec{
			{Name: "tokenizer.json", SHA256: sha256Hex(tokJSON)},
			{Name: "model.safetensors", SHA256: sha256Hex(stBytes)},
		},
	}
	dir, err := Install(context.Background(), veskaHome, "potion-test", spec)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if want := ModelDir(veskaHome, "potion-test"); dir != want {
		t.Errorf("Install dir: got %q want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "model.safetensors")); err != nil {
		t.Errorf("model.safetensors not present in returned dir: %v", err)
	}
}

// TestEnsureModel_ReusesCachedFiles verifies that files with matching checksums are not downloaded again on subsequent runs.
func TestEnsureModel_ReusesCachedFiles(t *testing.T) {
	tokJSON := []byte(`{"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0}}}`)
	stBytes := []byte("safetensors-bytes")
	hits := map[string]int{}
	srv := newFakeHFServerCounting(map[string][]byte{
		"/tokenizer.json":    tokJSON,
		"/model.safetensors": stBytes,
	}, hits)
	defer srv.Close()

	dir := t.TempDir()
	spec := ModelSpec{
		BaseURL: srv.URL,
		Files: []FileSpec{
			{Name: "tokenizer.json", SHA256: sha256Hex(tokJSON)},
			{Name: "model.safetensors", SHA256: sha256Hex(stBytes)},
		},
	}
	// First call: 2 fetches.
	if err := ensureModel(context.Background(), dir, spec); err != nil {
		t.Fatalf("ensureModel 1: %v", err)
	}
	if hits["/tokenizer.json"] != 1 || hits["/model.safetensors"] != 1 {
		t.Fatalf("first call hits: %+v", hits)
	}
	// Second call: should be a no-op.
	if err := ensureModel(context.Background(), dir, spec); err != nil {
		t.Fatalf("ensureModel 2: %v", err)
	}
	if hits["/tokenizer.json"] != 1 || hits["/model.safetensors"] != 1 {
		t.Errorf("second call refetched: %+v", hits)
	}
}

// TestEnsureModel_ShaMismatchTriggersRefetch verifies that files with mismatched checksums are redownloaded.
func TestEnsureModel_ShaMismatchTriggersRefetch(t *testing.T) {
	stBytes := []byte("safetensors-bytes")
	hits := map[string]int{}
	srv := newFakeHFServerCounting(map[string][]byte{
		"/model.safetensors": stBytes,
	}, hits)
	defer srv.Close()

	dir := t.TempDir()
	// Seed cache with WRONG content.
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := ModelSpec{
		BaseURL: srv.URL,
		Files: []FileSpec{
			{Name: "model.safetensors", SHA256: sha256Hex(stBytes)},
		},
	}
	if err := ensureModel(context.Background(), dir, spec); err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if hits["/model.safetensors"] != 1 {
		t.Errorf("corrupt cache should have triggered refetch; hits=%+v", hits)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if string(got) != string(stBytes) {
		t.Errorf("expected refetched bytes, got %q", got)
	}
}

// TestEnsureModel_DownloadFailureSurfaces verifies that server errors trigger failures and clean up partial downloads.
func TestEnsureModel_DownloadFailureSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	spec := ModelSpec{
		BaseURL: srv.URL,
		Files: []FileSpec{
			{Name: "model.safetensors", SHA256: "deadbeef"},
		},
	}
	err := ensureModel(context.Background(), dir, spec)
	if err == nil {
		t.Fatal("expected error from 500, got nil")
	}
	if !errors.Is(err, errDownloadFailed) {
		t.Errorf("error should wrap errDownloadFailed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "model.safetensors")); !os.IsNotExist(statErr) {
		t.Error("partial download left a file behind")
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newFakeHFServer(files map[string][]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
}

func newFakeHFServerCounting(files map[string][]byte, hits map[string]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
}
