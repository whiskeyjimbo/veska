package repo_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/repo"
)

// gitInitWithRemote initialises a real git working tree at dir, optionally adding an
// origin remote pointing at originURL. Returns dir or skips the test if
// git is unavailable.
func gitInitWithRemote(t *testing.T, dir, originURL string) {
	t.Helper()
	args := [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "user.email", "test@example.invalid"},
		{"-C", dir, "config", "user.name", "test"},
	}
	if originURL != "" {
		args = append(args, []string{"-C", dir, "remote", "add", "origin", originURL})
	}
	for _, a := range args {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", a, err, out)
		}
	}
}

func TestAdd_PopulatesCanonicalURLFromOrigin(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	db := newTestDB(t)
	dir := filepath.Join(t.TempDir(), "myrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInitWithRemote(t, dir, "git@github.com:foo/bar.git")

	id, existed, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if existed {
		t.Fatal("first add reported existed=true")
	}

	var canonical sql.NullString
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	// Input was the scp-like SSH form; stored value must be the canonical
	// https form so an https-form `search --repo <url>` matches.
	if !canonical.Valid || canonical.String != "https://github.com/foo/bar" {
		t.Errorf("canonical_url = %v, want canonicalised https://github.com/foo/bar", canonical)
	}
}

func TestAdd_NoOriginLeavesCanonicalURLNull(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	db := newTestDB(t)
	dir := filepath.Join(t.TempDir(), "noorigin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInitWithRemote(t, dir, "") // no origin remote

	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var canonical sql.NullString
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical.Valid {
		t.Errorf("canonical_url = %q, want NULL", canonical.String)
	}
}

func TestAdd_MalformedOriginLeavesCanonicalURLNull(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	db := newTestDB(t)
	dir := filepath.Join(t.TempDir(), "weirdorigin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// "garbage" is not a parseable git URL form — CanonicalURL rejects it,
	// detectOriginURL silently returns "", canonical_url stays NULL.
	gitInitWithRemote(t, dir, "garbage")

	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	var canonical sql.NullString
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical.Valid {
		t.Errorf("canonical_url = %q, want NULL for malformed origin", canonical.String)
	}
}

func TestAdd_ReAddDoesNotBackfillCanonicalURL(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	db := newTestDB(t)
	dir := filepath.Join(t.TempDir(), "readdrepo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// First Add: no origin yet → canonical_url NULL.
	gitInitWithRemote(t, dir, "")
	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Add an origin AFTER the first registration. Re-Add must NOT
	// backfill canonical_url — per pinned design, the alias is only
	// stamped on initial registration.
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/foo/bar").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	_, existed, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if !existed {
		t.Fatal("second Add: existed=false on the same path")
	}

	var canonical sql.NullString
	if err := db.QueryRow(`SELECT canonical_url FROM repos WHERE repo_id = ?`, id).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical.Valid {
		t.Errorf("canonical_url backfilled on re-Add = %q, want NULL", canonical.String)
	}
}
