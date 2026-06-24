// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	veskagit "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// TestIsShallow distinguishes a full repo from a --depth=1 clone of it - the
// exact shape URL-cloned repos take, which silently degrades the history tools.
func TestIsShallow(t *testing.T) {
	src := initRepoWithFile(t)
	// A second commit so a depth=1 clone genuinely truncates history.
	mustWriteFile(t, filepath.Join(src, "b.txt"), "two\n")
	runGit(t, src, "add", "b.txt")
	runGit(t, src, "commit", "-q", "-m", "second")

	ctx := context.Background()

	if shallow, err := veskagit.IsShallow(ctx, src); err != nil || shallow {
		t.Fatalf("full repo: IsShallow=%v err=%v, want false/nil", shallow, err)
	}

	// Shallow-clone via file:// so git honors --depth on a local path.
	dest := t.TempDir()
	clone := filepath.Join(dest, "clone")
	if out, err := exec.CommandContext(ctx, "git", "clone", "--depth=1", "file://"+src, clone).CombinedOutput(); err != nil {
		t.Fatalf("shallow clone: %v: %s", err, out)
	}

	if shallow, err := veskagit.IsShallow(ctx, clone); err != nil || !shallow {
		t.Fatalf("depth=1 clone: IsShallow=%v err=%v, want true/nil", shallow, err)
	}
}

// TestIsShallow_EmptyRoot guards the empty-root precondition.
func TestIsShallow_EmptyRoot(t *testing.T) {
	if _, err := veskagit.IsShallow(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty repoRoot")
	}
}
