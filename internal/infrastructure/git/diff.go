// Package git contains git-related infrastructure adapters.
// diff.go exposes ChangedFiles / ChangedFilesStaged — thin os/exec wrappers
// around `git diff --name-only` used by the eng_get_diff_blast_radius
// MCP tool to map a working-tree diff onto the set of touched files.
// Both helpers return paths relative to repoRoot — that is what
// `git diff --name-only` emits. The nodes table, however, stores
// file_path ABSOLUTE, so callers MUST join these paths against repoRoot
// before looking them up in storage (blastradius.DiffOf does this;
// was an empty-blast bug from skipping that step).
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ChangedFiles returns the list of files modified in the working tree
// relative to HEAD (i.e. unstaged + uncommitted changes that are NOT
// yet `git add`-ed). It is the working-tree complement of
// ChangedFilesStaged.
// An empty repoRoot returns an error rather than silently shelling out
// against the process cwd: blast-radius tools must always run scoped to
// a registered repo.
func ChangedFiles(ctx context.Context, repoRoot string) ([]string, error) {
	return runDiffNameOnly(ctx, repoRoot, false)
}

// ChangedFilesStaged returns the list of files staged for commit
// (i.e. `git diff --cached --name-only`). Used when the caller wants
// the blast radius of "what is about to be committed" rather than
// "what is dirty in the working tree".
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
	// A single empty line means "no changes"; collapse it to an empty slice.
	if len(out) == 1 && out[0] == "" {
		return nil, nil
	}
	return out, nil
}
