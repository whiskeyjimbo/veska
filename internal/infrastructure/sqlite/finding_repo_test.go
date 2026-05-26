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
		"repo1", "main",
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
		"repo1", "main",
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
		"repo1", "main",
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
			"repo1", "main",
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
		"repo1", "main",
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

// TestFindingRepo_CloseObsolete verifies that CloseObsolete flips an open
// finding to closed with closed_reason='revalidated_obsolete', and is a
// harmless no-op against a finding_id that does not exist.
func TestFindingRepo_CloseObsolete(t *testing.T) {
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

	f, err := domain.NewFinding(
		"repo1", "main",
		domain.SeverityMedium, domain.LayerStructural,
		"parse-failure", "tree-sitter could not parse foo.go",
		domain.WithFileAnchor("foo.go"),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	if err := repo.Save(context.Background(), f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := repo.CloseObsolete(context.Background(), f.FindingID, "main"); err != nil {
		t.Fatalf("CloseObsolete: %v", err)
	}

	var (
		gotState        string
		gotClosedReason sql.NullString
		gotClosedAt     sql.NullInt64
	)
	err = db.QueryRow(
		`SELECT state, closed_reason, closed_at FROM findings WHERE finding_id = ? AND branch = ?`,
		f.FindingID, "main",
	).Scan(&gotState, &gotClosedReason, &gotClosedAt)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	if gotState != "closed" {
		t.Errorf("state = %q, want closed", gotState)
	}
	if !gotClosedReason.Valid || gotClosedReason.String != "revalidated_obsolete" {
		t.Errorf("closed_reason = %v, want revalidated_obsolete", gotClosedReason)
	}
	if !gotClosedAt.Valid || gotClosedAt.Int64 <= 0 {
		t.Errorf("closed_at = %v, want a positive timestamp", gotClosedAt)
	}

	// Closing a non-existent finding is a no-op: no error, no row created.
	if err := repo.CloseObsolete(context.Background(), "doesnotexist", "main"); err != nil {
		t.Fatalf("CloseObsolete(non-existent): %v", err)
	}
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM findings`).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Errorf("findings count after no-op CloseObsolete: got %d, want 1", cnt)
	}
}

// TestFindingRepo_CloseSupersededAutoLinks covers solov2-ok7y: an UPDATE
// scoped by (repo_id, branch, rule='auto-link', state='open') and gated on
// the finding anchor referencing a SIMILAR_TO edge whose src is in the
// supplied source-node-id set. Rows that don't match are untouched.
func TestFindingRepo_CloseSupersededAutoLinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	ctx := context.Background()

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Seed source and target nodes so the edges FK is satisfied.
	for _, id := range []string{"n1", "n2", "tA", "tB", "tC"} {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "main", "r1", "go", "function", id, "x.go",
			1, 10, "h-"+id, now, "service:veska", "system"); err != nil {
			t.Fatalf("insert node %s: %v", id, err)
		}
	}

	// Insert three SIMILAR_TO edges: n1→tA, n1→tB, n2→tC.
	type edge struct{ id, src, dst string }
	edges := []edge{
		{id: "eA", src: "n1", dst: "tA"},
		{id: "eB", src: "n1", dst: "tB"},
		{id: "eC", src: "n2", dst: "tC"},
	}
	for _, e := range edges {
		if _, err := db.Exec(`INSERT INTO edges (
			edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
		) VALUES (?,?,?,?,?,?,?,?)`,
			e.id, "main", "r1", e.src, e.dst, "SIMILAR_TO", "unresolved", now); err != nil {
			t.Fatalf("insert edge %s: %v", e.id, err)
		}
	}

	repo := sqlite.NewFindingRepo(db)

	// Helper that builds + saves an auto-link finding anchored on an edge id.
	saveAutoLink := func(edgeID string) string {
		f, err := domain.NewFinding(
			"r1", "main",
			domain.SeverityLow, domain.LayerSemantic,
			"auto-link", "similar to "+edgeID,
			domain.WithNodeAnchor(edgeID),
		)
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		if err := repo.Save(ctx, f); err != nil {
			t.Fatalf("Save: %v", err)
		}
		return f.FindingID
	}

	fA := saveAutoLink("eA")
	fB := saveAutoLink("eB")
	fC := saveAutoLink("eC")

	// Insert a non-auto-link finding anchored on eA to verify the rule filter.
	other, err := domain.NewFinding(
		"r1", "main",
		domain.SeverityMedium, domain.LayerStructural,
		"dead-code", "dead",
		domain.WithNodeAnchor("eA"),
	)
	if err != nil {
		t.Fatalf("NewFinding dead-code: %v", err)
	}
	if err := repo.Save(ctx, other); err != nil {
		t.Fatalf("Save dead-code: %v", err)
	}

	// Empty source set is a no-op.
	if err := repo.CloseSupersededAutoLinks(ctx, "r1", "main", nil); err != nil {
		t.Fatalf("CloseSupersededAutoLinks(nil): %v", err)
	}
	for _, fid := range []string{fA, fB, fC} {
		assertState(t, db, fid, "main", "open")
	}

	// Supersede n1's auto-link findings only.
	if err := repo.CloseSupersededAutoLinks(ctx, "r1", "main", []string{"n1"}); err != nil {
		t.Fatalf("CloseSupersededAutoLinks([n1]): %v", err)
	}
	assertState(t, db, fA, "main", "closed")
	assertState(t, db, fB, "main", "closed")
	assertState(t, db, fC, "main", "open")
	assertState(t, db, other.FindingID, "main", "open") // dead-code untouched

	// Calling again is idempotent — no further state change.
	if err := repo.CloseSupersededAutoLinks(ctx, "r1", "main", []string{"n1"}); err != nil {
		t.Fatalf("CloseSupersededAutoLinks(idempotent): %v", err)
	}
	assertState(t, db, fA, "main", "closed")
	assertState(t, db, fC, "main", "open")
}

func assertState(t *testing.T, db *sql.DB, findingID, branch, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(
		`SELECT state FROM findings WHERE finding_id = ? AND branch = ?`,
		findingID, branch,
	).Scan(&got); err != nil {
		t.Fatalf("state lookup %s: %v", findingID, err)
	}
	if got != want {
		t.Errorf("finding %s state = %q, want %q", findingID, got, want)
	}
}
