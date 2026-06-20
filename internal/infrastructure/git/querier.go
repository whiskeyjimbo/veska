// SPDX-License-Identifier: AGPL-3.0-only

// querier.go exposes a Querier struct that gathers the read-only Git
// history queries needed by the application StartupResync process. Each method
// wraps Git command execution and aligns with repository-relative path conventions.

package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Querier implements the GitQuerier interface by executing the git CLI. It carries no state and uses a value receiver.
type Querier struct{}

// HEAD returns the current HEAD SHA for the repository. An empty rootPath returns an error to prevent running commands outside repository scope.
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

// IsAncestor reports whether the specified commit is reachable from the head reference. It returns an error if the underlying git merge-base command fails.
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

// CommitsSince returns the list of commit SHAs from the baseline reference up to head, sorted oldest first.
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

// ChangedFiles returns the files modified in the specified commit. The returned paths are relative to the repository root.
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

// ReadFileAtCommit retrieves the contents of the specified file at a given commit.
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

// runQuerierGit runs a context-less git command scoped to the specified repository root and returns its standard output.
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

// Conformance to the application.GitQuerier interface is verified in the daemon wire step to prevent hexagonal layering violations.
