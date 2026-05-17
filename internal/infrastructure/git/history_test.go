package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// runGitEnv runs a git command with extra environment variables and
// fails the test on error. Used to pin committer dates for windowing.
func runGitEnv(t *testing.T, args []string, extraEnv ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// commitFileAt writes content to path, stages it, and commits with an
// explicit author+committer date so windowing tests are deterministic.
func commitFileAt(t *testing.T, dir, name, content, msg string, when time.Time) {
	t.Helper()
	mustWriteFile(t, filepath.Join(dir, name), content)
	runGit(t, dir, "add", name)
	stamp := when.Format(time.RFC3339)
	cmd := []string{"-C", dir,
		"-c", "user.email=test@example.com",
		"-c", "user.name=Test",
		"-c", "commit.gpgsign=false",
		"commit", "-q", "-m", msg,
		"--date", stamp,
	}
	runGitEnv(t, cmd, "GIT_COMMITTER_DATE="+stamp)
}

func TestChangeCounts_WindowExcludesOldCommits(t *testing.T) {
	dir := initRepoWithFile(t)
	now := time.Now()
	// In-window: 2 commits to a.txt within the last 30 days.
	commitFileAt(t, dir, "a.txt", "v1\n", "recent 1", now.AddDate(0, 0, -1))
	commitFileAt(t, dir, "a.txt", "v2\n", "recent 2", now.AddDate(0, 0, -2))
	// Out-of-window: 100 days ago.
	commitFileAt(t, dir, "b.txt", "old\n", "old b", now.AddDate(0, 0, -100))

	counts, err := veskagit.ChangeCounts(context.Background(), dir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ChangeCounts: %v", err)
	}
	// init (current time) + 2 backdated edits all touch a.txt in window.
	if counts["a.txt"] != 3 {
		t.Errorf("a.txt: got %d want 3", counts["a.txt"])
	}
	if _, ok := counts["b.txt"]; ok {
		t.Errorf("b.txt should be out of window, got %d", counts["b.txt"])
	}
}

func TestChangeCounts_DefaultWindowIs30Days(t *testing.T) {
	dir := initRepoWithFile(t)
	now := time.Now()
	commitFileAt(t, dir, "a.txt", "v1\n", "recent", now.AddDate(0, 0, -5))
	commitFileAt(t, dir, "b.txt", "old\n", "old", now.AddDate(0, 0, -90))

	// Zero window selects the default (30 days).
	counts, err := veskagit.ChangeCounts(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("ChangeCounts: %v", err)
	}
	// init (current time) + the -5d edit are within the default window.
	if counts["a.txt"] != 2 {
		t.Errorf("a.txt: got %d want 2", counts["a.txt"])
	}
	if _, ok := counts["b.txt"]; ok {
		t.Errorf("b.txt should be excluded by default 30d window")
	}
}

func TestFileHistory_ListsRecentCommitsTouchingFile(t *testing.T) {
	dir := initRepoWithFile(t)
	now := time.Now()
	commitFileAt(t, dir, "a.txt", "v1\n", "edit a one", now.AddDate(0, 0, -3))
	commitFileAt(t, dir, "b.txt", "b\n", "edit b", now.AddDate(0, 0, -2))
	commitFileAt(t, dir, "a.txt", "v2\n", "edit a two", now.AddDate(0, 0, -1))

	commits, err := veskagit.FileHistory(context.Background(), dir, "a.txt", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("FileHistory: %v", err)
	}
	// init + 2 edits = 3 commits touching a.txt.
	if len(commits) != 3 {
		t.Fatalf("got %d commits want 3: %+v", len(commits), commits)
	}
	for _, c := range commits {
		if c.Hash == "" {
			t.Errorf("empty hash in %+v", c)
		}
		if c.Subject == "" {
			t.Errorf("empty subject in %+v", c)
		}
	}
	// Newest first by author date: init is committed at the real current
	// time, the edits are backdated, so init sorts ahead of them.
	if commits[0].Subject != "init" {
		t.Errorf("expected newest first (init), got %q", commits[0].Subject)
	}
	if commits[1].Subject != "edit a two" || commits[2].Subject != "edit a one" {
		t.Errorf("unexpected order: %q, %q", commits[1].Subject, commits[2].Subject)
	}
}

func TestFileHistory_WindowExcludesOldCommits(t *testing.T) {
	dir := initRepoWithFile(t)
	now := time.Now()
	// "old a" is 100 days back; "recent a" is in window. "init" (from
	// initRepoWithFile) is committed at the real current time so it is
	// also in window — expect those two, not the old one.
	commitFileAt(t, dir, "a.txt", "v1\n", "recent a", now.AddDate(0, 0, -2))
	commitFileAt(t, dir, "a.txt", "v2\n", "old a", now.AddDate(0, 0, -100))

	commits, err := veskagit.FileHistory(context.Background(), dir, "a.txt", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("FileHistory: %v", err)
	}
	for _, c := range commits {
		if c.Subject == "old a" {
			t.Errorf("out-of-window commit included: %q", c.Subject)
		}
	}
	if len(commits) != 2 {
		t.Fatalf("got %d want 2: %+v", len(commits), commits)
	}
}

func TestHistory_Deterministic(t *testing.T) {
	dir := initRepoWithFile(t)
	now := time.Now()
	commitFileAt(t, dir, "a.txt", "v1\n", "a one", now.AddDate(0, 0, -2))
	commitFileAt(t, dir, "a.txt", "v2\n", "a two", now.AddDate(0, 0, -1))

	c1, err := veskagit.ChangeCounts(context.Background(), dir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ChangeCounts: %v", err)
	}
	c2, err := veskagit.ChangeCounts(context.Background(), dir, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ChangeCounts: %v", err)
	}
	if len(c1) != len(c2) || c1["a.txt"] != c2["a.txt"] {
		t.Errorf("ChangeCounts not deterministic: %v vs %v", c1, c2)
	}

	h1, _ := veskagit.FileHistory(context.Background(), dir, "a.txt", 30*24*time.Hour)
	h2, _ := veskagit.FileHistory(context.Background(), dir, "a.txt", 30*24*time.Hour)
	if len(h1) != len(h2) {
		t.Fatalf("FileHistory length differs: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if h1[i] != h2[i] {
			t.Errorf("FileHistory[%d] differs: %+v vs %+v", i, h1[i], h2[i])
		}
	}
}

func TestChangeCounts_EmptyRepoRootIsError(t *testing.T) {
	if _, err := veskagit.ChangeCounts(context.Background(), "", 0); err == nil {
		t.Error("expected error for empty repoRoot")
	}
}

func TestFileHistory_EmptyArgsAreErrors(t *testing.T) {
	if _, err := veskagit.FileHistory(context.Background(), "", "a.txt", 0); err == nil {
		t.Error("expected error for empty repoRoot")
	}
	dir := initRepoWithFile(t)
	if _, err := veskagit.FileHistory(context.Background(), dir, "", 0); err == nil {
		t.Error("expected error for empty path")
	}
}
