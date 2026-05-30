//go:build network

// Network-tagged integration tests for the clone helper (solov2-kxo5.1).
// Excluded from `go test ./...` so the default loop stays offline-safe;
// run with `go test -tags=network ./internal/infrastructure/repo/`.

package repo_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// A small public repo that's unlikely to disappear; pick a fixed commit
// for deterministic clone size. octocat/Hello-World is github's canonical
// sample repo and trivially small.
const cloneTestRepoURL = "https://github.com/octocat/Hello-World.git"

func TestClone_PublicRepo(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "hello")
	var progress bytes.Buffer

	got, err := repo.Clone(t.Context(), cloneTestRepoURL, dest, &progress)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if got != dest {
		t.Errorf("returned path %q != dest %q", got, dest)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Errorf(".git missing in clone destination: %v", err)
	}
	if progress.Len() == 0 {
		t.Error("--progress produced no stderr output")
	}
}

func TestClone_FailureSurfacesStderr(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "nope")
	var progress bytes.Buffer

	_, err := repo.Clone(t.Context(),
		"https://github.com/this-org-definitely-does-not-exist-kxo5/no-such-repo.git",
		dest, &progress)
	if err == nil {
		t.Fatal("expected clone of nonexistent repo to fail")
	}
	// AC3: git's stderr must be present verbatim in the error message.
	// The exact wording varies (404, authentication, terminal-progress
	// noise) so just assert that some non-trivial stderr made it through.
	if !strings.Contains(err.Error(), "git clone") {
		t.Errorf("err missing 'git clone' prefix: %v", err)
	}
	if len(err.Error()) < len("git clone : ")+20 {
		t.Errorf("err too short to contain meaningful stderr: %v", err)
	}
}
