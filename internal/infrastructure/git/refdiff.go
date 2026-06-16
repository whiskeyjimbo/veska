// refdiff.go exposes thin os/exec wrappers used by the
// eng_find_changed_symbols MCP tool to compare two arbitrary git refs
// on demand: ChangedFilesBetween lists the files that differ between
// two refs, and FileAtRef reads a single file's content at a ref.
// Like diff.go, paths are relative to repoRoot — that is what
// `git diff --name-only` emits and what `git show <ref>:<path>` expects.

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ErrFileNotAtRef is returned by FileAtRef when the requested path does
// not exist at the given ref (e.g. the file was added after ref_a or
// deleted before ref_b). Callers treat this as "symbol set is empty at
// that ref" rather than a hard failure.
var ErrFileNotAtRef = errors.New("git show: file not present at ref")

// ErrUnknownRevision is returned by ChangedFilesBetween when one of the
// refs does not resolve in the repo — most commonly HEAD~1 on a
// freshly-init'd repo with a single commit, but also typos and stale
// branch names. Callers (e.g. the eng_find_changed_symbols MCP tool)
// translate this into a typed invalid-params response rather than
// leaking raw git stderr to the wire.
var ErrUnknownRevision = errors.New("git diff: unknown revision")

// ChangedFilesBetween returns the list of files that differ between
// refA and refB, as `git diff --name-only <refA> <refB>` reports them.
// Paths are relative to repoRoot.
// An empty repoRoot or an empty ref returns an error rather than
// silently shelling out against the process cwd or HEAD.
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
		// Map the common "ref doesn't resolve" failure (ambiguous argument /
		// unknown revision) to a typed error so callers can return a clean
		// invalid-params response instead of leaking raw git stderr.
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

// WorkingTreeHasUncommittedChanges reports whether repoRoot has any
// uncommitted modifications (staged, unstaged, or untracked Go-ish source
// files) by running `git status --porcelain`. Returns false on any
// shell-out error so callers don't surface a false "dirty" signal from a
// transient git failure. Used by post-promotion-oriented tools
// (eng_find_todos) to add a degraded_reason explaining why
// working-tree edits are invisible to them.
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

// ResolvesRef reports whether ref resolves to a commit in repoRoot via
// `git rev-parse --verify <ref>^{commit}`. Used by callers that need to
// say "ref_a is the bad one" after ChangedFilesBetween returned
// ErrUnknownRevision — git's combined error doesn't say which side
// failed.
func ResolvesRef(ctx context.Context, repoRoot, ref string) bool {
	if repoRoot == "" || ref == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return cmd.Run() == nil
}

// FileAtRef returns the content of path as it existed at ref, via
// `git show <ref>:<path>`. Path is relative to repoRoot.
// When the file does not exist at ref, ErrFileNotAtRef is returned and
// the content is nil — this is expected for files added or deleted
// between the two refs being compared.
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
		// `git show` fails with a non-zero exit when the path is absent
		// at the ref; treat that as ErrFileNotAtRef so callers can skip
		// the missing side of an added/deleted file gracefully.
		return nil, fmt.Errorf("%w: %s:%s in %s: %v: %s",
			ErrFileNotAtRef, ref, path, repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
