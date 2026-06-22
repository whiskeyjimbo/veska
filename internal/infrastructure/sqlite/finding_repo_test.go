// SPDX-License-Identifier: AGPL-3.0-only

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

func TestFindingRepo_SaveBatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	repo := sqlite.NewFindingRepo(db)

	mk := func(anchor string) *domain.Finding {
		f, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerSemantic, Rule: "auto-link", Message: "m"}, domain.WithNodeAnchor(anchor))
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		return f
	}
	batch := []*domain.Finding{mk("e1"), mk("e2"), nil, mk("e3")} // nil is skipped

	if err := repo.SaveBatch(context.Background(), batch); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM findings WHERE rule='auto-link'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if got := count(); got != 3 {
		t.Fatalf("after SaveBatch: want 3 findings, got %d", got)
	}
	// Idempotent: re-saving the same batch upserts, does not duplicate.
	if err := repo.SaveBatch(context.Background(), batch); err != nil {
		t.Fatalf("SaveBatch (2nd): %v", err)
	}
	if got := count(); got != 3 {
		t.Fatalf("after 2nd SaveBatch: want 3 (idempotent), got %d", got)
	}
	// Empty batch is a no-op, no error.
	if err := repo.SaveBatch(context.Background(), nil); err != nil {
		t.Fatalf("SaveBatch(nil): %v", err)
	}
}

func TestFindingRepo_SaveRoundTrip(t *testing.T) {
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

	f, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "parse-failure", Message: "tree-sitter could not parse foo.go"}, domain.WithFileAnchor("foo.go"))
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}

	if err := repo.Save(context.Background(), f); err != nil {
		t.Fatalf("Save: %v", err)
	}

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
	fWith, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "msg"}, domain.WithNodeAnchor("n-with"),
		domain.WithAnchorContentHash("h-1"),
	)
	if err != nil {
		t.Fatalf("NewFinding with hash: %v", err)
	}
	if err := repo.Save(context.Background(), fWith); err != nil {
		t.Fatalf("Save with hash: %v", err)
	}

	// Without hash (parse-failure style).
	fWithout, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "parse-failure", Message: "msg"}, domain.WithFileAnchor("foo.go"))
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
		f, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "msg"}, domain.WithNodeAnchor("n-x"),
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

	f, _ := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: "dead-code", Message: "no inbound edges"}, domain.WithNodeAnchor("n1"))

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

	f, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: "parse-failure", Message: "tree-sitter could not parse foo.go"}, domain.WithFileAnchor("foo.go"))
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

	saveAutoLink := func(edgeID string) string {
		f, err := domain.NewFinding(domain.FindingSpec{RepoID: "r1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerSemantic, Rule: "auto-link", Message: "similar to " + edgeID}, domain.WithNodeAnchor(edgeID))
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

	other, err := domain.NewFinding(domain.FindingSpec{RepoID: "r1", Branch: "main", Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: "dead-code", Message: "dead"}, domain.WithNodeAnchor("eA"))
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

	// Calling again is idempotent - no further state change.
	if err := repo.CloseSupersededAutoLinks(ctx, "r1", "main", []string{"n1"}); err != nil {
		t.Fatalf("CloseSupersededAutoLinks(idempotent): %v", err)
	}
	assertState(t, db, fA, "main", "closed")
	assertState(t, db, fC, "main", "open")
}

func TestFindingRepo_CloseSupersededByRule(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	ctx := context.Background()

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	repo := sqlite.NewFindingRepo(db)

	save := func(rule, key string) string {
		f, err := domain.NewFinding(domain.FindingSpec{RepoID: "r1", Branch: "main", Severity: domain.SeverityHigh, Layer: domain.LayerSecurity, Rule: rule, Message: "msg " + key}, domain.WithFileAnchor("go.mod"),
			domain.WithFindingKey(key),
		)
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		if err := repo.Save(ctx, f); err != nil {
			t.Fatalf("Save: %v", err)
		}
		return f.FindingID
	}

	// Three vuln findings exist from a prior scan; one will remain
	// applicable on rescan, two will be superseded.
	keep := save("vulnerable_dependency", "GHSA-keep-jwt-go")
	gone1 := save("vulnerable_dependency", "GHSA-gone-yaml-1")
	gone2 := save("vulnerable_dependency", "GHSA-gone-yaml-2")
	// A finding under a different rule that must NOT be touched.
	otherRule := save("secret_leak", "rule-isolation-marker")

	if err := repo.CloseSupersededByRule(ctx, "r1", "main", "vulnerable_dependency", []string{keep}); err != nil {
		t.Fatalf("CloseSupersededByRule: %v", err)
	}

	assertState(t, db, keep, "main", "open")
	assertState(t, db, gone1, "main", "closed")
	assertState(t, db, gone2, "main", "closed")
	assertState(t, db, otherRule, "main", "open")

	// Idempotency: a second call with the same keep is a no-op.
	if err := repo.CloseSupersededByRule(ctx, "r1", "main", "vulnerable_dependency", []string{keep}); err != nil {
		t.Fatalf("CloseSupersededByRule (idempotent): %v", err)
	}
	assertState(t, db, keep, "main", "open")

	// Empty keep closes everything in scope (e.g. dep removed entirely).
	if err := repo.CloseSupersededByRule(ctx, "r1", "main", "vulnerable_dependency", nil); err != nil {
		t.Fatalf("CloseSupersededByRule(empty keep): %v", err)
	}
	assertState(t, db, keep, "main", "closed")
	// Other-rule finding still untouched after the wipe.
	assertState(t, db, otherRule, "main", "open")
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
