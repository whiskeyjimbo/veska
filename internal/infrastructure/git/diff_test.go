// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// runGit runs a git command in the specified directory, failing the test if the execution encounters an error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// runGitOut runs a git command in the specified directory, failing the test on error and returning the trimmed stdout.
func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := exec.Command("sh", "-c", "printf %s '"+content+"' > "+path).Run(); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// initRepoWithFile initializes a temporary Git repository with a single committed file,
// ensuring subsequent edits register as working-tree changes relative to HEAD.
func initRepoWithFile(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	mustWriteFile(t, filepath.Join(dir, "a.txt"), "hello\n")
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestChangedFiles_ReportsWorkingTreeDiff(t *testing.T) {
	dir := initRepoWithFile(t)
	// Modify an existing tracked file without staging it.
	mustWriteFile(t, filepath.Join(dir, "a.txt"), "world\n")

	got, err := veskagit.ChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	sort.Strings(got)
	want := []string{"a.txt"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestChangedFiles_CleanTreeReturnsEmpty(t *testing.T) {
	dir := initRepoWithFile(t)
	got, err := veskagit.ChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestChangedFilesStaged_ReportsCachedDiff(t *testing.T) {
	dir := initRepoWithFile(t)
	mustWriteFile(t, filepath.Join(dir, "a.txt"), "staged\n")
	runGit(t, dir, "add", "a.txt")

	got, err := veskagit.ChangedFilesStaged(context.Background(), dir)
	if err != nil {
		t.Fatalf("ChangedFilesStaged: %v", err)
	}
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("got %v want [a.txt]", got)
	}

	// Ensure the working-tree diff is clean once the changes are staged.
	gotWT, err := veskagit.ChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(gotWT) != 0 {
		t.Errorf("expected empty working-tree diff after add, got %v", gotWT)
	}
}

func TestChangedFiles_EmptyRepoRootIsError(t *testing.T) {
	_, err := veskagit.ChangedFiles(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty repoRoot")
	}
}
