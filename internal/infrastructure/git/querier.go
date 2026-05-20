// querier.go exposes a Querier struct that gathers the read-only git
// history queries needed by application.StartupResync into a single value
// satisfying the application.GitQuerier port. Each method shells out via
// os/exec the same way the existing free functions in this package do
// (diff.go, refdiff.go, history.go); behaviour is intentionally identical
// so callers can rely on the same path conventions (repoRoot-relative).

package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Querier implements application.GitQuerier by shelling out to the
// `git` CLI. It carries no state — methods are pinned to a value receiver
// so callers can pass either &Querier{} or Querier{} interchangeably.
type Querier struct{}

// HEAD returns the current HEAD SHA for the repo at rootPath.
//
// An empty rootPath returns an error rather than silently shelling out
// against the process cwd: history queries must always run scoped to a
// registered repo.
func (Querier) HEAD(rootPath string) (string, error) {
	if rootPath == "" {
		return "", fmt.Errorf("git rev-parse: repoRoot is empty")
	}
	out, err := runQuerierGit(rootPath, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IsAncestor reports whether sha is reachable from head — i.e. sha is in
// HEAD's commit history. Implemented with `git merge-base --is-ancestor`,
// which exits 0 (ancestor) or 1 (not an ancestor); other exit codes
// surface as errors.
func (Querier) IsAncestor(rootPath, sha, head string) (bool, error) {
	if rootPath == "" {
		return false, fmt.Errorf("git merge-base: repoRoot is empty")
	}
	if sha == "" || head == "" {
		return false, fmt.Errorf("git merge-base: sha and head must be non-empty (got %q, %q)", sha, head)
	}
	cmd := exec.Command("git", "-C", rootPath, "merge-base", "--is-ancestor", sha, head)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, fmt.Errorf("git merge-base in %s: %w: %s", rootPath, err, strings.TrimSpace(stderr.String()))
}

// CommitsSince returns the list of commit SHAs from sha..head in
// oldest-first order — the output of `git log <sha>..<head> --reverse
// --format=%H`. The set excludes sha itself.
func (Querier) CommitsSince(rootPath, sha, head string) ([]string, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("git log: repoRoot is empty")
	}
	if sha == "" || head == "" {
		return nil, fmt.Errorf("git log: sha and head must be non-empty (got %q, %q)", sha, head)
	}
	out, err := runQuerierGit(rootPath, "log", "--reverse", "--format=%H", sha+".."+head)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// ChangedFiles returns the files modified in the given commit SHA — i.e.
// `git diff-tree --no-commit-id --name-only -r <sha>`. Paths are
// repoRoot-relative, matching the convention of ChangedFiles in diff.go.
func (Querier) ChangedFiles(rootPath, sha string) ([]string, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("git diff-tree: repoRoot is empty")
	}
	if sha == "" {
		return nil, fmt.Errorf("git diff-tree: sha is empty")
	}
	out, err := runQuerierGit(rootPath, "diff-tree", "--no-commit-id", "--name-only", "-r", sha)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// ReadFileAtCommit returns the contents of filePath as it existed at sha,
// via `git show <sha>:<path>`. The error surface mirrors FileAtRef in
// refdiff.go for missing paths.
func (Querier) ReadFileAtCommit(rootPath, sha, filePath string) ([]byte, error) {
	if rootPath == "" {
		return nil, fmt.Errorf("git show: repoRoot is empty")
	}
	if sha == "" || filePath == "" {
		return nil, fmt.Errorf("git show: sha and path must be non-empty (got %q, %q)", sha, filePath)
	}
	cmd := exec.Command("git", "-C", rootPath, "show", sha+":"+filePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s in %s: %w: %s",
			sha, filePath, rootPath, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runQuerierGit runs a context-less git command scoped to rootPath and
// returns stdout. The Querier interface methods do not accept a ctx
// argument (application.GitQuerier is ctx-less by design — see
// application/resync.go), so cancellation is not threaded here.
func runQuerierGit(rootPath string, args ...string) (string, error) {
	full := append([]string{"-C", rootPath}, args...)
	cmd := exec.Command("git", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %v in %s: %w: %s",
			args, rootPath, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Note: importing internal/application from infrastructure/git would
// violate hexagonal layering, so no compile-time interface assertion
// lives here. Contract conformance is verified by composition in
// cmd/veska-daemon/wire.go.
