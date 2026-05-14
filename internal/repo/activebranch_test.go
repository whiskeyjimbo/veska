package repo_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/repo"
)

// TestSetActiveBranch verifies that SetActiveBranch stores the branch name.
func TestSetActiveBranch(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	id, err := repo.Add(context.Background(), db, dir)
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

// TestSetActiveBranchUpdates verifies that a second call overwrites the first.
func TestSetActiveBranchUpdates(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	id, err := repo.Add(context.Background(), db, dir)
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

// TestSetActiveBranchUnknownRepo verifies that an unknown repoID is a silent no-op.
// Unregistered repos (e.g. repos that haven't been added with `engram repo add`)
// are ignored — the hook must never block a checkout.
func TestSetActiveBranchUnknownRepo(t *testing.T) {
	db := newTestDB(t)

	if err := repo.SetActiveBranch(context.Background(), db, "nonexistent-id", "main"); err != nil {
		t.Errorf("SetActiveBranch with unknown repo should be a no-op, got: %v", err)
	}
}
