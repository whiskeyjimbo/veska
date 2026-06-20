// SPDX-License-Identifier: AGPL-3.0-only

package doctor_test

import (
	"database/sql"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// createReposDB creates a SQLite DB at path carrying just the repos columns the
// identity probe reads.
func createReposDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqldriver.Name, path)
	if err != nil {
		t.Fatalf("createReposDB: open: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE repos (
		repo_id        TEXT PRIMARY KEY,
		root_path      TEXT NOT NULL UNIQUE,
		identity_tier  TEXT
	)`)
	if err != nil {
		t.Fatalf("createReposDB: create table: %v", err)
	}
	return db
}

func insertRepo(t *testing.T, db *sql.DB, id, root, tier string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, identity_tier) VALUES (?, ?, ?)`,
		id, root, tier,
	); err != nil {
		t.Fatalf("insertRepo: %v", err)
	}
}

// TestCheckIdentityTiersAllConverging verifies that repos on module-hostpath
// report healthy with zero non-converging.
func TestCheckIdentityTiersAllConverging(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createReposDB(t, path)
	insertRepo(t, db, "a", "/x/a", "module-hostpath")
	insertRepo(t, db, "b", "/x/b", "module-hostpath")
	db.Close()

	report, err := doctor.CheckIdentityTiers(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want healthy", report.Status)
	}
	if report.NonConverging != 0 {
		t.Errorf("NonConverging: got %d, want 0", report.NonConverging)
	}
	if len(report.Repos) != 2 {
		t.Errorf("Repos: got %d, want 2", len(report.Repos))
	}
}

// TestCheckIdentityTiersNonConverging verifies that a repo on a non-converging
// tier (and an unresolved one) is flagged degraded.
func TestCheckIdentityTiersNonConverging(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createReposDB(t, path)
	insertRepo(t, db, "good", "/x/good", "module-hostpath")
	insertRepo(t, db, "url", "/x/url", "origin-url")
	insertRepo(t, db, "bare", "/x/bare", "module-bare")
	insertRepo(t, db, "abs", "/x/abs", "abs-root")
	insertRepo(t, db, "old", "/x/old", "") // pre-0018, unresolved
	db.Close()

	report, err := doctor.CheckIdentityTiers(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "degraded" {
		t.Errorf("Status: got %q, want degraded", report.Status)
	}
	// origin-url, module-bare, abs-root, and unresolved all fail to converge.
	if report.NonConverging != 4 {
		t.Errorf("NonConverging: got %d, want 4", report.NonConverging)
	}
	for _, r := range report.Repos {
		wantConverge := r.RepoID == "good"
		if r.Converges != wantConverge {
			t.Errorf("repo %s Converges: got %v, want %v", r.RepoID, r.Converges, wantConverge)
		}
	}
}

// TestCheckIdentityTiersNoRepos verifies an empty repos table reports healthy.
func TestCheckIdentityTiersNoRepos(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db := createReposDB(t, path)
	db.Close()

	report, err := doctor.CheckIdentityTiers(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want healthy", report.Status)
	}
}

// TestCheckIdentityTiersNoReposTable verifies a DB without the repos table
// (some fixtures) reports healthy/empty rather than broken.
func TestCheckIdentityTiersNoReposTable(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := sql.Open(sqldriver.Name, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE other (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	db.Close()

	report, err := doctor.CheckIdentityTiers(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want healthy", report.Status)
	}
}

// TestCheckIdentityTiersMissingDB verifies a nonexistent DB reports broken
// (never a panic / os.Exit).
func TestCheckIdentityTiersMissingDB(t *testing.T) {
	report, err := doctor.CheckIdentityTiers(t.TempDir() + "/does-not-exist.db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want broken", report.Status)
	}
}
