// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// refdiff.go provides helpers to compare Git references and read file contents at specific references.

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrFileNotAtRef is returned when the requested file path does not exist at the specified reference.
var ErrFileNotAtRef = errors.New("git show: file not present at ref")

// ErrUnknownRevision is returned when one of the provided Git references cannot be resolved.
var ErrUnknownRevision = errors.New("git diff: unknown revision")

// ChangedFilesBetween returns the list of files that differ between two references.
// The returned paths are relative to the repository root. An empty repoRoot or ref returns
// an error to prevent running commands outside repository scope.
func ChangedFilesBetween(ctx context.Context, repoRoot, refA, refB string) ([]string, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git diff: repoRoot is empty")
	}
	if refA == "" || refB == "" {
		return nil, fmt.Errorf("git diff: both refs must be non-empty (got %q, %q)", refA, refB)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "diff", "--name-only", refA, refB)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Map unresolved reference failures to a typed error to simplify caller handling.
		stderrStr := stderr.String()
		if strings.Contains(stderrStr, "ambiguous argument") || strings.Contains(stderrStr, "unknown revision") {
			return nil, fmt.Errorf("%w: refs=%s..%s", ErrUnknownRevision, refA, refB)
		}
		return nil, fmt.Errorf("git diff %s..%s in %s: %w: %s",
			refA, refB, repoRoot, err, strings.TrimSpace(stderrStr))
	}
	out := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil, nil
	}
	return out, nil
}

// WorkingTreeHasUncommittedChanges reports whether the repository contains any uncommitted
// changes (staged, unstaged, or untracked). It returns false if the git command fails to
// prevent reporting false positives on transient errors.
func WorkingTreeHasUncommittedChanges(ctx context.Context, repoRoot string) bool {
	if repoRoot == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "status", "--porcelain")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false
	}
	return strings.TrimSpace(stdout.String()) != ""
}

// ResolvesRef reports whether a Git reference successfully resolves to a commit in the repository.
func ResolvesRef(ctx context.Context, repoRoot, ref string) bool {
	if repoRoot == "" || ref == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return cmd.Run() == nil
}

// FileAtRef returns the content of a file as it existed at the specified reference.
// The file path is relative to the repository root. It returns ErrFileNotAtRef if the path
// is absent at that reference.
func FileAtRef(ctx context.Context, repoRoot, ref, path string) ([]byte, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("git show: repoRoot is empty")
	}
	if ref == "" || path == "" {
		return nil, fmt.Errorf("git show: ref and path must be non-empty (got %q, %q)", ref, path)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "show", ref+":"+path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// An execution failure indicates the path is absent at the reference, which we map to ErrFileNotAtRef.
		return nil, fmt.Errorf("%w: %s:%s in %s: %v: %s",
			ErrFileNotAtRef, ref, path, repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
