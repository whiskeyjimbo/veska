// SPDX-License-Identifier: AGPL-3.0-only

package repo_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// gitInitWithRemote initializes a Git working tree and configures an optional remote URL.
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
	// The stored URL must match the canonical HTTPS format even when initialized with SSH.
	if !canonical.Valid || canonical.String != "https://github.com/foo/bar" {
		t.Errorf("canonical_url = %v, want canonicalized https://github.com/foo/bar", canonical)
	}
}

func TestAdd_NoOriginLeavesCanonicalURLNull(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	db := newTestDB(t)
	dir := filepath.Join(t.TempDir(), "noorigin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInitWithRemote(t, dir, "") // No remote origin URL is configured.

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
	// Malformed URLs are rejected during canonicalization, leaving the canonical URL as NULL.
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
	// Perform initial registration without a Git remote, resulting in a NULL canonical URL.
	gitInitWithRemote(t, dir, "")
	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Verify that subsequent registrations do not retrospectively update canonical URLs.
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
