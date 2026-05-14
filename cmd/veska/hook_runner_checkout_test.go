package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPostCheckoutIsGitSpecialState verifies that postCheckoutCmd skips work
// when a MERGE_HEAD file is present (the helper is shared with post-commit).
func TestPostCheckoutIsGitSpecialState(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isGitSpecialState(dir) {
		t.Error("expected isGitSpecialState to return true for MERGE_HEAD, got false")
	}
}
