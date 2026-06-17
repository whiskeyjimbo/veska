//go:build network

// Network-tagged integration tests for the clone helper are excluded from
// standard test runs to maintain offline safety.

package repo_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// A small, stable public repository URL used for network integration tests.
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
	// Git's standard error output must be returned verbatim within the error message.
	if !strings.Contains(err.Error(), "git clone") {
		t.Errorf("err missing 'git clone' prefix: %v", err)
	}
	if len(err.Error()) < len("git clone : ")+20 {
		t.Errorf("err too short to contain meaningful stderr: %v", err)
	}
}
