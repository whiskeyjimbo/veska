package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestFindingRepo_SaveRoundTrip verifies a Finding can be saved via the port
// adapter and re-read with source_layer='structural'.
func TestFindingRepo_SaveRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))

	// FK constraint: insert a repo row first.
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	repo := sqlite.NewFindingRepo(db)

	f, err := domain.NewFinding(
		"01HXYZ", "repo1", "main",
		domain.SeverityLow, domain.LayerStructural,
		"parse-failure", "tree-sitter could not parse foo.go",
		domain.WithFileAnchor("foo.go"),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}

	if err := repo.Save(context.Background(), f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify row is present with source_layer='structural'.
	var (
		gotSrcLayer string
		gotRule     string
		gotState    string
		gotMessage  string
	)
	err = db.QueryRow(
		`SELECT source_layer, rule, state, message FROM findings WHERE finding_id = ? AND branch = ?`,
		f.FindingID, f.Branch,
	).Scan(&gotSrcLayer, &gotRule, &gotState, &gotMessage)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	if gotSrcLayer != "structural" {
		t.Errorf("source_layer = %q, want structural", gotSrcLayer)
	}
	if gotRule != "parse-failure" {
		t.Errorf("rule = %q, want parse-failure", gotRule)
	}
	if gotState != "open" {
		t.Errorf("state = %q, want open", gotState)
	}
	if gotMessage != "tree-sitter could not parse foo.go" {
		t.Errorf("message = %q", gotMessage)
	}
}

// TestFindingRepo_Idempotent verifies that saving the same finding twice does
// not error (UPSERT semantics).
func TestFindingRepo_Idempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	repo := sqlite.NewFindingRepo(db)

	f, _ := domain.NewFinding(
		"id1", "repo1", "main",
		domain.SeverityMedium, domain.LayerStructural,
		"dead-code", "no inbound edges",
		domain.WithNodeAnchor("n1"),
	)

	if err := repo.Save(context.Background(), f); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := repo.Save(context.Background(), f); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	var cnt int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM findings WHERE finding_id = ?`, f.FindingID,
	).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Errorf("findings count after 2 saves: got %d, want 1", cnt)
	}
}
