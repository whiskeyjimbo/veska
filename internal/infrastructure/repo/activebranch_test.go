// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

// gitInit creates an empty .git/hooks directory inside dir so that repo.Add
// accepts it as a valid git work-tree.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
}

// TestSetActiveBranch verifies that SetActiveBranch correctly writes the active branch name to the database.
func TestSetActiveBranch(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	gitInit(t, dir)
	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := repo.SetActiveBranch(context.Background(), db, id, "main"); err != nil {
		t.Fatalf("SetActiveBranch: %v", err)
	}

	var branch string
	if err := db.QueryRow(
		"SELECT active_branch FROM repos WHERE repo_id = ?", id,
	).Scan(&branch); err != nil {
		t.Fatalf("query: %v", err)
	}
	if branch != "main" {
		t.Errorf("active_branch = %q, want %q", branch, "main")
	}
}

// TestSetActiveBranchUpdates verifies that subsequent calls overwrite the previously stored branch name.
func TestSetActiveBranchUpdates(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	gitInit(t, dir)
	id, _, err := repo.Add(context.Background(), db, dir)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := repo.SetActiveBranch(context.Background(), db, id, "main"); err != nil {
		t.Fatalf("SetActiveBranch (main): %v", err)
	}
	if err := repo.SetActiveBranch(context.Background(), db, id, "feature/foo"); err != nil {
		t.Fatalf("SetActiveBranch (feature/foo): %v", err)
	}

	var branch string
	if err := db.QueryRow(
		"SELECT active_branch FROM repos WHERE repo_id = ?", id,
	).Scan(&branch); err != nil {
		t.Fatalf("query: %v", err)
	}
	if branch != "feature/foo" {
		t.Errorf("active_branch = %q, want %q", branch, "feature/foo")
	}
}

// TestSetActiveBranchUnknownRepo verifies that updating an unknown repository ID is a silent no-op
// to ensure the post-checkout hook does not fail or block git checkouts for unregistered repositories.
func TestSetActiveBranchUnknownRepo(t *testing.T) {
	db := newTestDB(t)

	if err := repo.SetActiveBranch(context.Background(), db, "nonexistent-id", "main"); err != nil {
		t.Errorf("SetActiveBranch with unknown repo should be a no-op, got: %v", err)
	}
}
