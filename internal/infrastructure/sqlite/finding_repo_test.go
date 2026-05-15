package sqlite_test

import (
	"context"
	"database/sql"
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

// TestFindingRepo_AnchorContentHash_RoundTrip verifies that a non-nil
// anchor_content_hash on a domain.Finding survives INSERT and reads back
// identically, while a nil value reads back as SQL NULL.
func TestFindingRepo_AnchorContentHash_RoundTrip(t *testing.T) {
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

	// With hash.
	fWith, err := domain.NewFinding(
		"u-with", "repo1", "main",
		domain.SeverityLow, domain.LayerStructural,
		"dead-code", "msg",
		domain.WithNodeAnchor("n-with"),
		domain.WithAnchorContentHash("h-1"),
	)
	if err != nil {
		t.Fatalf("NewFinding with hash: %v", err)
	}
	if err := repo.Save(context.Background(), fWith); err != nil {
		t.Fatalf("Save with hash: %v", err)
	}

	// Without hash (parse-failure style).
	fWithout, err := domain.NewFinding(
		"u-no", "repo1", "main",
		domain.SeverityLow, domain.LayerStructural,
		"parse-failure", "msg",
		domain.WithFileAnchor("foo.go"),
	)
	if err != nil {
		t.Fatalf("NewFinding without hash: %v", err)
	}
	if err := repo.Save(context.Background(), fWithout); err != nil {
		t.Fatalf("Save without hash: %v", err)
	}

	var gotHash sql.NullString
	if err := db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id = ? AND branch = ?`,
		fWith.FindingID, fWith.Branch,
	).Scan(&gotHash); err != nil {
		t.Fatalf("query with-hash: %v", err)
	}
	if !gotHash.Valid || gotHash.String != "h-1" {
		t.Errorf("anchor_content_hash with-hash = %+v, want h-1", gotHash)
	}

	if err := db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id = ? AND branch = ?`,
		fWithout.FindingID, fWithout.Branch,
	).Scan(&gotHash); err != nil {
		t.Fatalf("query without-hash: %v", err)
	}
	if gotHash.Valid {
		t.Errorf("anchor_content_hash without-hash should be NULL, got %q", gotHash.String)
	}
}

// TestFindingRepo_AnchorContentHash_OnConflictRefreshes verifies that re-saving
// a finding with a DIFFERENT anchor_content_hash UPDATEs the column so the
// revalidation sweep sees the new hash. This is the drift-propagation path.
func TestFindingRepo_AnchorContentHash_OnConflictRefreshes(t *testing.T) {
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

	build := func(hash string) *domain.Finding {
		f, err := domain.NewFinding(
			"u", "repo1", "main",
			domain.SeverityLow, domain.LayerStructural,
			"dead-code", "msg",
			domain.WithNodeAnchor("n-x"),
			domain.WithAnchorContentHash(hash),
		)
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		return f
	}

	first := build("h-old")
	if err := repo.Save(context.Background(), first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := build("h-new")
	if err := repo.Save(context.Background(), second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if first.FindingID != second.FindingID {
		t.Fatalf("finding_id should be stable across hash change: %q vs %q",
			first.FindingID, second.FindingID)
	}

	var got sql.NullString
	if err := db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id = ? AND branch = ?`,
		first.FindingID, first.Branch,
	).Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !got.Valid || got.String != "h-new" {
		t.Errorf("after re-save: anchor_content_hash = %+v, want h-new", got)
	}

	// Exactly one row survives (idempotency).
	var cnt int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM findings WHERE finding_id = ?`, first.FindingID,
	).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Errorf("rows after re-save: got %d, want 1", cnt)
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
