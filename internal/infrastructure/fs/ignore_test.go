package fs_test

import (
	"os"
	"path/filepath"
	"testing"

	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
)

func TestLoad_NoFile_ReturnsDefaults(t *testing.T) {
	dir := t.TempDir()

	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	patterns := il.Patterns()
	if len(patterns) == 0 {
		t.Fatal("expected at least one pattern (defaults), got none")
	}

	// Check a few known defaults are present
	want := map[string]bool{
		"vendor/":       false,
		"node_modules/": false,
		".git/":         false,
	}
	for _, p := range patterns {
		want[p] = true
	}
	for k, found := range want {
		if !found {
			t.Errorf("expected default pattern %q to be present", k)
		}
	}
}

func TestLoad_WithFile_MergesDefaultsAndFile(t *testing.T) {
	dir := t.TempDir()
	content := "custom-dir/\n# this is a comment\n\n*.tmp\n"
	if err := os.WriteFile(filepath.Join(dir, ".veskaignore"), []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	patterns := il.Patterns()
	have := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		have[p] = true
	}

	// defaults must still be present
	if !have["vendor/"] {
		t.Error("expected default pattern vendor/ to be present after merge")
	}
	// user patterns must be present
	if !have["custom-dir/"] {
		t.Error("expected user pattern custom-dir/ to be present")
	}
	if !have["*.tmp"] {
		t.Error("expected user pattern *.tmp to be present")
	}
	// comment and blank line must NOT be patterns
	for _, p := range patterns {
		if p == "" || p[0] == '#' {
			t.Errorf("unexpected pattern %q: comments and blank lines should be skipped", p)
		}
	}
}

func TestShouldIgnore_VendorDir(t *testing.T) {
	dir := t.TempDir()
	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !il.ShouldIgnore("vendor/foo.go") {
		t.Error("expected vendor/foo.go to be ignored")
	}
}

func TestShouldIgnore_NodeModules(t *testing.T) {
	dir := t.TempDir()
	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !il.ShouldIgnore("node_modules/lodash/index.js") {
		t.Error("expected node_modules/lodash/index.js to be ignored")
	}
}

func TestShouldIgnore_RegularFile_NotIgnored(t *testing.T) {
	dir := t.TempDir()
	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if il.ShouldIgnore("src/main.go") {
		t.Error("expected src/main.go to NOT be ignored")
	}
}

func TestShouldIgnore_CustomPattern(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".veskaignore"), []byte("secrets/\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !il.ShouldIgnore("secrets/passwords.txt") {
		t.Error("expected secrets/passwords.txt to be ignored by custom pattern")
	}
	if il.ShouldIgnore("src/app.go") {
		t.Error("expected src/app.go to NOT be ignored")
	}
}

func TestLoad_CommentsAndBlankLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	content := "# header comment\n\nkeep-me/\n  # indented comment\n\n"
	if err := os.WriteFile(filepath.Join(dir, ".veskaignore"), []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	have := make(map[string]bool)
	for _, p := range il.Patterns() {
		have[p] = true
	}

	if !have["keep-me/"] {
		t.Error("expected keep-me/ to be in patterns")
	}
	for _, p := range il.Patterns() {
		if p == "" {
			t.Error("blank line should not appear as pattern")
		}
	}
}

func TestShouldIgnore_GlobPattern(t *testing.T) {
	dir := t.TempDir()
	// *.pb.go is a default pattern
	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !il.ShouldIgnore("foo.pb.go") {
		t.Error("expected foo.pb.go to be ignored by *.pb.go glob pattern")
	}
	if !il.ShouldIgnore("internal/gen/bar.pb.go") {
		t.Error("expected internal/gen/bar.pb.go to be ignored by *.pb.go glob pattern")
	}
	if il.ShouldIgnore("foo.go") {
		t.Error("expected foo.go to NOT be ignored")
	}
}

// TestShouldIgnore_AgentWorktrees guards solov2-v2zx: AI-agent worktree roots
// (.claude/worktrees/, .cursor/, .aider*/) are skipped by default so cold
// scans don't index N duplicate copies of every symbol — one per worktree.
func TestShouldIgnore_AgentWorktrees(t *testing.T) {
	dir := t.TempDir()
	il, err := fsignore.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	skip := []string{
		".claude/worktrees/agent-abc/cmd/foo/main.go",
		".claude/settings.json",
		".cursor/rules/main.go",
		".aider.tags.cache.v3/something.go",
	}
	for _, p := range skip {
		if !il.ShouldIgnore(p) {
			t.Errorf("expected %q to be ignored under default agent-worktree patterns", p)
		}
	}
	// Sanity: real source files outside those roots still scan.
	for _, p := range []string{"main.go", "internal/api/server.go"} {
		if il.ShouldIgnore(p) {
			t.Errorf("expected %q to NOT be ignored", p)
		}
	}
}
