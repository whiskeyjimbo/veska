package repo_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// makeLocalBareRepo initializes a real Git repository in a temporary directory, makes a
// single empty commit, and returns the absolute path to the directory.
func makeLocalBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "user.email", "test@example.invalid"},
		{"-C", dir, "config", "user.name", "test"},
		{"-C", dir, "commit", "--allow-empty", "-m", "init", "-q"},
	} {
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable: %v: %s", args, err, out)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestTrackedClonePath(t *testing.T) {
	t.Parallel()

	a := repo.TrackedClonePath("/tmp/veska", "https://github.com/foo/bar")
	b := repo.TrackedClonePath("/tmp/veska", "https://github.com/foo/bar")
	c := repo.TrackedClonePath("/tmp/veska", "https://github.com/foo/baz")

	if a != b {
		t.Errorf("not deterministic for same input: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("collision across distinct URLs: %q == %q", a, c)
	}
	if !filepath.IsAbs(a) {
		t.Errorf("not absolute: %q", a)
	}
	if filepath.Dir(a) != "/tmp/veska/repos" {
		t.Errorf("wrong parent: %q", filepath.Dir(a))
	}
}

func TestSetCanonicalURL_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"abc123", "/tmp/foo", int64(1000),
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := repo.SetCanonicalURL(context.Background(), db, "abc123", "git@github.com:foo/bar.git"); err != nil {
		t.Fatalf("SetCanonicalURL: %v", err)
	}

	var got string
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, "abc123").Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	// Stored value must be the canonical HTTPS representation.
	if got != "https://github.com/foo/bar" {
		t.Errorf("canonical_url = %q, want canonicalised https form", got)
	}
}

func TestLookupByCanonicalURL_HitAndMiss(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, canonical_url) VALUES (?, ?, ?, ?)`,
		"abc123", "/tmp/foo", int64(1000), "https://github.com/foo/bar",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Lookup via raw SSH format must successfully match the stored canonical HTTPS representation.
	got, ok, err := repo.LookupByCanonicalURL(context.Background(), db, "git@github.com:foo/bar.git")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || got.RepoID != "abc123" {
		t.Errorf("hit: ok=%v repo_id=%q want abc123", ok, got.RepoID)
	}

	// Verify a lookup on an unregistered URL returns a negative result.
	_, ok, err = repo.LookupByCanonicalURL(context.Background(), db, "https://gitlab.com/x/y")
	if err != nil {
		t.Fatalf("miss query: %v", err)
	}
	if ok {
		t.Error("miss: ok=true, want false")
	}

	// Verify that malformed URLs trigger an error.
	_, _, err = repo.LookupByCanonicalURL(context.Background(), db, "not-a-url")
	if !errors.Is(err, repo.ErrInvalidURL) {
		t.Errorf("invalid URL err = %v, want ErrInvalidURL", err)
	}
}

func TestAddFromURL_IdempotentOnExistingCanonicalURL(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, canonical_url) VALUES (?, ?, ?, ?)`,
		"preexisting-id", "/tmp/preexisting", int64(1000), "https://github.com/foo/bar",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	home := t.TempDir()
	id, existed, err := repo.AddFromURL(context.Background(), db, home,
		"git@github.com:foo/bar.git", nil)
	if err != nil {
		t.Fatalf("AddFromURL: %v", err)
	}
	if id != "preexisting-id" || !existed {
		t.Errorf("got (%q, existed=%v), want (preexisting-id, true)", id, existed)
	}
	// Ensure no new repository directory was cloned.
	if entries, _ := os.ReadDir(filepath.Join(home, "repos")); len(entries) != 0 {
		t.Errorf("expected no clone dir; got %d entries under %s/repos", len(entries), home)
	}
}

func TestAddFromURL_EndToEnd_LocalFileURL(t *testing.T) {
	source := makeLocalBareRepo(t)
	db := newTestDB(t)
	home := t.TempDir()

	id, existed, err := repo.AddFromURL(context.Background(), db, home,
		"file://"+source, nil)
	if err != nil {
		t.Fatalf("AddFromURL: %v", err)
	}
	if existed {
		t.Errorf("first add: existed=true, want false")
	}
	if id == "" {
		t.Fatal("empty repo_id")
	}

	// Verify that the registered repository record stamps the canonical URL.
	var canonical string
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, id).Scan(&canonical); err != nil {
		t.Fatalf("read canonical_url: %v", err)
	}
	if canonical != "file://"+source {
		t.Errorf("canonical_url = %q, want %q", canonical, "file://"+source)
	}

	// Re-running the registration should skip clone operations by matching the canonical URL.
	id2, existed2, err := repo.AddFromURL(context.Background(), db, home,
		"file://"+source, nil)
	if err != nil {
		t.Fatalf("second AddFromURL: %v", err)
	}
	if id2 != id || !existed2 {
		t.Errorf("idempotent: got (%q, existed=%v), want (%q, true)", id2, existed2, id)
	}
}

func TestAddFromURL_RollsBackOnCloneFailure(t *testing.T) {
	db := newTestDB(t)
	home := t.TempDir()

	// Using a nonexistent local file URL triggers an immediate clone failure.
	bogus := "file://" + filepath.Join(home, "no-such-source")

	_, _, err := repo.AddFromURL(context.Background(), db, home, bogus, nil)
	if err == nil {
		t.Fatal("expected clone failure")
	}

	// Ensure no repository database records were created.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rows after failed add: %d, want 0", n)
	}
	// Verify that any partial clone directories were cleaned up.
	if entries, _ := os.ReadDir(filepath.Join(home, "repos")); len(entries) != 0 {
		t.Errorf("orphan clone dirs: %d under %s/repos", len(entries), home)
	}
}
