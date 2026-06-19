// SPDX-License-Identifier: AGPL-3.0-only

package wikicmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// setupRepo creates a migrated veska.db under a temp VESKA_HOME with a single
// registered repo and returns the open DB, the repo root, and the repo_id.
func setupRepo(t *testing.T) (db *sql.DB, repoRoot, repoID string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	repoRoot = t.TempDir()

	dbPath := filepath.Join(home, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repoID = "repo-under-test"
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO repos (repo_id, root_path, added_at, active_branch, module_path)
		 VALUES (?, ?, ?, ?, ?)`,
		repoID, repoRoot, time.Now().Unix(), "main", sql.NullString{String: "example.com/m", Valid: true},
	)
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return db, repoRoot, repoID
}

// TestResolveTarget exercises the --repo / --branch resolution rules.
func TestResolveTarget(t *testing.T) {
	db, repoRoot, repoID := setupRepo(t)
	ctx := context.Background()

	t.Run("defaults to sole repo and its branch", func(t *testing.T) {
		gotRepo, gotBranch, err := ResolveTarget(ctx, db, "", "")
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		if gotRepo != repoID {
			t.Errorf("repo: got %q want %q", gotRepo, repoID)
		}
		if gotBranch != "main" {
			t.Errorf("branch: got %q want %q", gotBranch, "main")
		}
	})

	t.Run("explicit branch overrides default", func(t *testing.T) {
		_, gotBranch, err := ResolveTarget(ctx, db, repoID, "feature")
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		if gotBranch != "feature" {
			t.Errorf("branch: got %q want %q", gotBranch, "feature")
		}
	})

	t.Run("unknown repo errors", func(t *testing.T) {
		if _, _, err := ResolveTarget(ctx, db, "nope", ""); err == nil {
			t.Error("expected error for unregistered repo")
		}
	})

	// positional path arg resolves the repo by RootPath.
	t.Run("registered path resolves to repo", func(t *testing.T) {
		gotRepo, _, err := ResolveTarget(ctx, db, repoRoot, "")
		if err != nil {
			t.Fatalf("ResolveTarget(path): %v", err)
		}
		if gotRepo != repoID {
			t.Errorf("repo: got %q want %q", gotRepo, repoID)
		}
	})

	t.Run("unregistered existing path errors clearly", func(t *testing.T) {
		other := t.TempDir()
		_, _, err := ResolveTarget(ctx, db, other, "")
		if err == nil {
			t.Fatal("expected error for unregistered path")
		}
		if !strings.Contains(err.Error(), "not a registered repository") {
			t.Errorf("want %q in err, got %v", "not a registered repository", err)
		}
	})
}
