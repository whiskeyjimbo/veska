// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ChangedFiles returns the list of files modified in the working tree relative to HEAD (unstaged
// and uncommitted changes). The returned paths are relative to the repository root, so callers
// must resolve them to absolute paths if matching against absolute storage paths. An empty
// repoRoot returns an error to prevent running commands outside a registered repository scope.
func ChangedFiles(ctx context.Context, repoRoot string) ([]string, error) {
	return runDiffNameOnly(ctx, repoRoot, false)
}

// ChangedFilesStaged returns the list of files staged for commit. The returned paths are
// relative to the repository root, so callers must resolve them to absolute paths if matching
// against absolute storage paths.
func ChangedFilesStaged(ctx context.Context, repoRoot string) ([]string, error) {
	return runDiffNameOnly(ctx, repoRoot, true)
}

func runDiffNameOnly(ctx context.Context, repoRoot string, cached bool) ([]string, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git diff: repoRoot is empty")
	}
	args := []string{"-C", repoRoot, "diff", "--name-only"}
	if cached {
		args = append(args, "--cached")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff in %s: %w: %s", repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	out := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	// An empty output line indicates that no changes were detected, which we normalize to a nil slice.
	if len(out) == 1 && out[0] == "" {
		return nil, nil
	}
	return out, nil
}
