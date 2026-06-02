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

// runGitDeterministic runs a git subcommand in root with a deterministic
// identity and frozen author/committer dates, failing the test (with combined
// output) on any error. (The package's other runGit helper does not pin
// identity/dates, so coverage fixtures use this one.)
func runGitDeterministic(t *testing.T, root string, args ...string) {
	t.Helper()
	full := append([]string{
		"-C", root,
		"-c", "user.email=test@example.com",
		"-c", "user.name=test",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+fixtureGitDate,
		"GIT_COMMITTER_DATE="+fixtureGitDate,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
