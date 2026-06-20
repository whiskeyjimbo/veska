// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"path/filepath"
	"testing"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// TestQuerier_HEAD verifies that the Querier reports the working-tree HEAD SHA for an initialized repository.
func TestQuerier_HEAD(t *testing.T) {
	dir := initRepoWithFile(t)
	want := runGitOut(t, dir, "rev-parse", "HEAD")

	var q veskagit.Querier
	got, err := q.HEAD(dir)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if got != want {
		t.Fatalf("HEAD = %q; want %q", got, want)
	}
}

// TestQuerier_HEAD_EmptyRoot ensures that an empty repository root path causes the call to return an error.
func TestQuerier_HEAD_EmptyRoot(t *testing.T) {
	var q veskagit.Querier
	if _, err := q.HEAD(""); err == nil {
		t.Fatal("expected error on empty repoRoot")
	}
}

// TestQuerier_IsAncestor verifies that reachable commits return true while unrelated commits return false.
func TestQuerier_IsAncestor(t *testing.T) {
	dir := initRepoWithFile(t)
	first := runGitOut(t, dir, "rev-parse", "HEAD")
	// Add a second commit so that the first commit is a proper ancestor of HEAD.
	mustWriteFile(t, filepath.Join(dir, "b.txt"), "second\n")
	runGit(t, dir, "add", "b.txt")
	runGit(t, dir, "commit", "-q", "-m", "second")
	head := runGitOut(t, dir, "rev-parse", "HEAD")

	var q veskagit.Querier
	ok, err := q.IsAncestor(dir, first, head)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !ok {
		t.Fatalf("IsAncestor(first, HEAD) = false; want true")
	}

	// Create a divergent branch whose commits are not ancestors of the main branch's HEAD.
	runGit(t, dir, "checkout", "-q", "-b", "other", first)
	mustWriteFile(t, filepath.Join(dir, "z.txt"), "divergent\n")
	runGit(t, dir, "add", "z.txt")
	runGit(t, dir, "commit", "-q", "-m", "divergent")
	other := runGitOut(t, dir, "rev-parse", "HEAD")
	runGit(t, dir, "checkout", "-q", "main")

	ok, err = q.IsAncestor(dir, other, head)
	if err != nil {
		t.Fatalf("IsAncestor divergent: %v", err)
	}
	if ok {
		t.Fatalf("IsAncestor(divergent, HEAD) = true; want false")
	}
}

// TestQuerier_CommitsSince_AndChangedFiles verifies that CommitsSince returns oldest-first commits and ChangedFiles lists modified paths.
func TestQuerier_CommitsSince_AndChangedFiles(t *testing.T) {
	dir := initRepoWithFile(t)
	first := runGitOut(t, dir, "rev-parse", "HEAD")

	mustWriteFile(t, filepath.Join(dir, "b.txt"), "second\n")
	runGit(t, dir, "add", "b.txt")
	runGit(t, dir, "commit", "-q", "-m", "second")
	second := runGitOut(t, dir, "rev-parse", "HEAD")

	mustWriteFile(t, filepath.Join(dir, "c.txt"), "third\n")
	runGit(t, dir, "add", "c.txt")
	runGit(t, dir, "commit", "-q", "-m", "third")
	head := runGitOut(t, dir, "rev-parse", "HEAD")

	var q veskagit.Querier
	commits, err := q.CommitsSince(dir, first, head)
	if err != nil {
		t.Fatalf("CommitsSince: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("commits = %v; want 2 entries", commits)
	}
	if commits[0] != second || commits[1] != head {
		t.Fatalf("commits = %v; want [%s %s]", commits, second, head)
	}

	files, err := q.ChangedFiles(dir, second)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(files) != 1 || files[0] != "b.txt" {
		t.Fatalf("ChangedFiles(second) = %v; want [b.txt]", files)
	}
}

// TestQuerier_ReadFileAtCommit verifies that the contents of a committed file can be read at a specific commit SHA.
func TestQuerier_ReadFileAtCommit(t *testing.T) {
	dir := initRepoWithFile(t)
	head := runGitOut(t, dir, "rev-parse", "HEAD")

	var q veskagit.Querier
	got, err := q.ReadFileAtCommit(dir, head, "a.txt")
	if err != nil {
		t.Fatalf("ReadFileAtCommit: %v", err)
	}
	if string(got) != "hello\n" {
		t.Fatalf("contents = %q; want %q", string(got), "hello\n")
	}
}
