package git_test

import (
	"context"
	"path/filepath"
	"testing"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// TestAddedLinesForCommit_ReportsOnlyAddedLines verifies that the parser extracts only newly introduced
// lines with correct line numbers, ignoring unmodified context or deleted lines.
func TestAddedLinesForCommit_ReportsOnlyAddedLines(t *testing.T) {
	dir := initRepoWithFile(t) // Initialize the repository with a text file.

	// Create and commit the initial Go source file.
	mustWriteFile(t, filepath.Join(dir, "code.go"), "package p\nfunc A() {}\nfunc B() {}\n")
	runGit(t, dir, "add", "code.go")
	runGit(t, dir, "commit", "-q", "-m", "add code")

	// Modify the Go source file and append a new line to trigger changes.
	mustWriteFile(t, filepath.Join(dir, "code.go"),
		"package p\nfunc A() { _ = 1 }\nfunc B() {}\nfunc C() {}\n")
	mustWriteFile(t, filepath.Join(dir, "a.txt"), "hello\nworld\n")
	runGit(t, dir, "add", "code.go", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "modify")
	sha := revParse(t, dir, "HEAD")

	got, err := veskagit.AddedLinesForCommit(context.Background(), dir, sha)
	if err != nil {
		t.Fatalf("AddedLinesForCommit: %v", err)
	}

	wantCode := []veskagit.Line{
		{Number: 2, Text: "func A() { _ = 1 }"},
		{Number: 4, Text: "func C() {}"},
	}
	assertLines(t, "code.go", got["code.go"], wantCode)

	wantTxt := []veskagit.Line{{Number: 2, Text: "world"}}
	assertLines(t, "a.txt", got["a.txt"], wantTxt)
}

// TestAddedLinesForCommit_RootCommit verifies that analyzing the initial commit of a repository
// treats all lines in the file as added.
func TestAddedLinesForCommit_RootCommit(t *testing.T) {
	dir := initRepoWithFile(t)
	sha := revParse(t, dir, "HEAD")

	got, err := veskagit.AddedLinesForCommit(context.Background(), dir, sha)
	if err != nil {
		t.Fatalf("AddedLinesForCommit: %v", err)
	}
	assertLines(t, "a.txt", got["a.txt"], []veskagit.Line{{Number: 1, Text: "hello"}})
}

func TestAddedLinesForCommit_EmptyArgsError(t *testing.T) {
	if _, err := veskagit.AddedLinesForCommit(context.Background(), "", "abc"); err == nil {
		t.Error("empty repoRoot: want error, got nil")
	}
	if _, err := veskagit.AddedLinesForCommit(context.Background(), "x", ""); err == nil {
		t.Error("empty commitSHA: want error, got nil")
	}
}

func assertLines(t *testing.T, file string, got, want []veskagit.Line) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d lines %v, want %d %v", file, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s line[%d] = %+v, want %+v", file, i, got[i], want[i])
		}
	}
}
