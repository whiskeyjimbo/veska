package repo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// TestRepoIDForPath_MatchesAddForNormalPath locks the equivalence the git
// hook runner depends on: the id RepoIDForPath derives must equal the id
// repo.Add stored for the same path. Without it, post-checkout SetActiveBranch
// would key on a different id and silently update zero rows.
func TestRepoIDForPath_MatchesAddForNormalPath(t *testing.T) {
	db := newTestDB(t)
	dir := newGitRepo(t)

	stored, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := repo.RepoIDForPath(dir); got != stored {
		t.Errorf("RepoIDForPath(%q) = %q, want stored id %q", dir, got, stored)
	}
}

// TestRepoIDForPath_MatchesAddThroughSymlink is the case the canonicalization
// fix was written for: when the working tree is reached through a symlinked
// path, RepoIDForPath must still resolve to the registry's stored id (which is
// computed from the symlink-resolved real path). A naive sha256 of the raw
// path would diverge here.
func TestRepoIDForPath_MatchesAddThroughSymlink(t *testing.T) {
	db := newTestDB(t)
	realDir := newGitRepo(t)

	// A symlink that points at the real repo dir. Register through the real
	// path (as `repo add` would after canonicalising), then look the id up
	// through the symlinked path the hook might see.
	link := filepath.Join(t.TempDir(), "link-to-repo")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	stored, _, err := repo.Add(context.Background(), db, realDir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := repo.RepoIDForPath(link); got != stored {
		t.Errorf("RepoIDForPath(symlink %q) = %q, want stored id %q", link, got, stored)
	}
}
