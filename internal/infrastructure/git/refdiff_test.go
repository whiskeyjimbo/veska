// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// initRepoTwoCommits initializes a temporary repository with two commits and returns
// the directory and both commit SHAs.
func initRepoTwoCommits(t *testing.T) (dir, refA, refB string) {
	t.Helper()
	dir = initRepoWithFile(t) // a.txt committed as "init"
	mustWriteFile(t, filepath.Join(dir, "code.go"), "package p\nfunc A() {}\n")
	runGit(t, dir, "add", "code.go")
	runGit(t, dir, "commit", "-q", "-m", "add code")
	refA = revParse(t, dir, "HEAD")

	mustWriteFile(t, filepath.Join(dir, "code.go"), "package p\nfunc A() { _ = 1 }\n")
	mustWriteFile(t, filepath.Join(dir, "new.go"), "package p\nfunc B() {}\n")
	runGit(t, dir, "add", "code.go", "new.go")
	runGit(t, dir, "commit", "-q", "-m", "modify and add")
	refB = revParse(t, dir, "HEAD")
	return dir, refA, refB
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out := runGitOut(t, dir, "rev-parse", ref)
	return out
}

func TestChangedFilesBetween_ListsDiff(t *testing.T) {
	dir, refA, refB := initRepoTwoCommits(t)
	got, err := veskagit.ChangedFilesBetween(context.Background(), dir, refA, refB)
	if err != nil {
		t.Fatalf("ChangedFilesBetween: %v", err)
	}
	sort.Strings(got)
	want := []string{"code.go", "new.go"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestChangedFilesBetween_SameRefEmpty(t *testing.T) {
	dir, _, refB := initRepoTwoCommits(t)
	got, err := veskagit.ChangedFilesBetween(context.Background(), dir, refB, refB)
	if err != nil {
		t.Fatalf("ChangedFilesBetween: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestChangedFilesBetween_EmptyArgsError(t *testing.T) {
	if _, err := veskagit.ChangedFilesBetween(context.Background(), "", "a", "b"); err == nil {
		t.Error("expected error for empty repoRoot")
	}
	if _, err := veskagit.ChangedFilesBetween(context.Background(), "x", "", "b"); err == nil {
		t.Error("expected error for empty refA")
	}
}

func TestFileAtRef_ReadsContentAtEachRef(t *testing.T) {
	dir, refA, refB := initRepoTwoCommits(t)

	a, err := veskagit.FileAtRef(context.Background(), dir, refA, "code.go")
	if err != nil {
		t.Fatalf("FileAtRef refA: %v", err)
	}
	if string(a) != "package p\nfunc A() {}\n" {
		t.Errorf("refA content = %q", a)
	}

	b, err := veskagit.FileAtRef(context.Background(), dir, refB, "code.go")
	if err != nil {
		t.Fatalf("FileAtRef refB: %v", err)
	}
	if string(b) != "package p\nfunc A() { _ = 1 }\n" {
		t.Errorf("refB content = %q", b)
	}
}

func TestFileAtRef_AbsentFileIsErrFileNotAtRef(t *testing.T) {
	dir, refA, _ := initRepoTwoCommits(t)
	// new.go was added in the second commit, so it should be absent at the first commit reference.
	_, err := veskagit.FileAtRef(context.Background(), dir, refA, "new.go")
	if !errors.Is(err, veskagit.ErrFileNotAtRef) {
		t.Fatalf("expected ErrFileNotAtRef, got %v", err)
	}
}
