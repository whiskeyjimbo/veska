package daemon

// Shared git-fixture helpers for TestToolCoverage rows whose tools diff or
// inspect real git history (eng_find_changed_symbols, eng_promote_repo,
// eng_get_diff_blast_radius, eng_get_dirty_blast_radius, eng_get_hot_zones).
//
// The committed testdata fixtures live inside the veska repo, so their git
// state is non-deterministic. These helpers build a throwaway repo with a
// fixed identity, fixed default branch, and fixed author/committer dates so
// the resulting commit graph is byte-for-byte reproducible across machines
// and CI runs.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fixtureGitDate is the frozen author/committer timestamp every helper-driven
// commit carries, so commit SHAs and ordering are deterministic.
const fixtureGitDate = "2025-01-01T00:00:00Z"

// initGitRepo creates a temp dir, runs `git init -b main` with a fixed
// identity, and returns the repo root. The branch is pinned so it does not
// depend on the host's init.defaultBranch config.
func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGitDeterministic(t, root, "init", "-b", "main")
	return root
}

// writeRepoFile writes content to root/rel, creating any parent directories.
func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// gitCommitAll stages everything and commits with msg under a fixed identity
// and frozen author/committer dates.
func gitCommitAll(t *testing.T, root, msg string) {
	t.Helper()
	runGitDeterministic(t, root, "add", "-A")
	runGitDeterministic(t, root, "commit", "-m", msg)
}

// gitCommitAllNow stages everything and commits with msg under the same fixed
// identity as gitCommitAll but WITHOUT pinning the author/committer dates, so
// git stamps the current wall-clock time. Tools whose ranking depends on a
// recent look-back window (eng_get_hot_zone uses now − 30 days) need commits
// inside that window, which the frozen fixtureGitDate (2025-01-01) is not.
func gitCommitAllNow(t *testing.T, root, msg string) {
	t.Helper()
	runGitNow(t, root, "add", "-A")
	runGitNow(t, root, "commit", "-m", msg)
}

// runGitDeterministic runs a git subcommand in root with a deterministic
// identity and frozen author/committer dates, failing the test (with combined
// output) on any error. (The package's other runGit helper does not pin
// identity/dates, so coverage fixtures use this one.)
func runGitDeterministic(t *testing.T, root string, args ...string) {
	t.Helper()
	runGitWithDate(t, root, fixtureGitDate, args...)
}

// runGitNow runs a git subcommand in root with the deterministic identity but
// the host's current time for author/committer dates.
func runGitNow(t *testing.T, root string, args ...string) {
	t.Helper()
	runGitWithDate(t, root, "", args...)
}

// runGitWithDate runs a git subcommand in root under the fixed coverage
// identity. When date is non-empty it pins GIT_AUTHOR_DATE/GIT_COMMITTER_DATE
// (deterministic SHAs); when empty git uses the current wall-clock time.
func runGitWithDate(t *testing.T, root, date string, args ...string) {
	t.Helper()
	full := append([]string{
		"-C", root,
		"-c", "user.email=test@example.com",
		"-c", "user.name=test",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = os.Environ()
	if date != "" {
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_DATE="+date,
			"GIT_COMMITTER_DATE="+date,
		)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
