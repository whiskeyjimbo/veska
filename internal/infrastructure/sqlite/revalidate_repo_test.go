package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// revalFixture wires a temp DB with a single repo row so the FK on findings
// is satisfied. It exposes typed helpers so tests stay focused on what the
// revalidate repo returns / mutates, not on SQL boilerplate.
type revalFixture struct {
	db       *sql.DB
	repoID   string
	branch   string
	findRepo *sqlite.FindingRepo
	reval    *sqlite.RevalidateRepo
}

func setupRevalFixture(t *testing.T) *revalFixture {
	t.Helper()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return &revalFixture{
		db:       db,
		repoID:   "repo1",
		branch:   "main",
		findRepo: sqlite.NewFindingRepo(db),
		reval:    sqlite.NewRevalidateRepo(db),
	}
}

func (f *revalFixture) insertNode(t *testing.T, nodeID, branch, filePath, contentHash string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, branch, f.repoID, "go", "function", nodeID, filePath,
		1, 10, contentHash, time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func (f *revalFixture) insertFinding(t *testing.T, id, branch, nodeID string, anchorHash *string) *domain.Finding {
	t.Helper()
	opts := []domain.FindingOption{domain.WithNodeAnchor(nodeID)}
	if anchorHash != nil {
		opts = append(opts, domain.WithAnchorContentHash(*anchorHash))
	}
	fnd, err := domain.NewFinding(
		id, f.repoID, branch,
		domain.SeverityLow, domain.LayerStructural,
		"dead-code", "msg",
		opts...,
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	if err := f.findRepo.Save(context.Background(), fnd); err != nil {
		t.Fatalf("Save finding: %v", err)
	}
	return fnd
}

//go:fix inline
func ptr(s string) *string { return new(s) }

func TestRevalidateRepo_StaleFindings_ReturnsOnlyDrift(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)

	// Two nodes on the same file; one has drifted, one matches the recorded anchor.
	f.insertNode(t, "n-stale", f.branch, "pkg/a.go", "h-current")
	f.insertNode(t, "n-fresh", f.branch, "pkg/a.go", "h-fresh")

	stale := f.insertFinding(t, "u-stale", f.branch, "n-stale", new("h-anchor-old"))
	_ = f.insertFinding(t, "u-fresh", f.branch, "n-fresh", new("h-fresh"))

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stale, want 1: %+v", len(got), got)
	}
	if got[0].FindingID != stale.FindingID {
		t.Errorf("FindingID = %q, want %q", got[0].FindingID, stale.FindingID)
	}
	if got[0].AnchorHash != "h-anchor-old" {
		t.Errorf("AnchorHash = %q, want h-anchor-old", got[0].AnchorHash)
	}
	if got[0].CurrentHash != "h-current" {
		t.Errorf("CurrentHash = %q, want h-current", got[0].CurrentHash)
	}
}

func TestRevalidateRepo_StaleFindings_SkipsNullAnchor(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	// Finding with NULL anchor_content_hash (file-anchored / parse-failure style).
	_ = f.insertFinding(t, "u-null", f.branch, "n-x", nil)

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 stale (NULL anchor must be skipped), got %d: %+v", len(got), got)
	}
}

func TestRevalidateRepo_StaleFindings_SkipsAlreadyClosed(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	fnd := f.insertFinding(t, "u-closed", f.branch, "n-x", new("h-old"))

	// Force-close it directly so the SELECT must observe state='open'.
	if _, err := f.db.Exec(
		`UPDATE findings SET state='closed', closed_reason='manual', closed_at=? WHERE finding_id=? AND branch=?`,
		time.Now().UnixMilli(), fnd.FindingID, fnd.Branch,
	); err != nil {
		t.Fatalf("force-close: %v", err)
	}

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 stale (closed must be skipped), got %d", len(got))
	}
}

func TestRevalidateRepo_StaleFindings_FileScoped(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-a", f.branch, "pkg/a.go", "h-current-a")
	f.insertNode(t, "n-b", f.branch, "pkg/b.go", "h-current-b")
	_ = f.insertFinding(t, "u-a", f.branch, "n-a", new("h-old-a"))
	_ = f.insertFinding(t, "u-b", f.branch, "n-b", new("h-old-b"))

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.FindingID)
	}
	sort.Strings(ids)
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("ids = %v, want exactly 1 finding from pkg/a.go", ids)
	}
}

func TestRevalidateRepo_StaleFindings_BranchScoped(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	// Same logical node lives on two branches with different current hashes.
	f.insertNode(t, "n-x", "main", "pkg/a.go", "h-main")
	f.insertNode(t, "n-x", "feature", "pkg/a.go", "h-feature")

	// On main: drift. On feature: also drift but should not be returned by a main query.
	_ = f.insertFinding(t, "u-main", "main", "n-x", new("h-old-main"))
	_ = f.insertFinding(t, "u-feat", "feature", "n-x", new("h-old-feat"))

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, "main", "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stale, want 1: %+v", len(got), got)
	}
	if got[0].FindingID == "" || got[0].CurrentHash != "h-main" {
		t.Errorf("expected main-branch row with current=h-main, got %+v", got[0])
	}
}

func TestRevalidateRepo_StaleFindings_RepoScoped(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	// Add a second repo with a same-named node + file.
	if _, err := f.db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo2", "/tmp/repo2", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo2: %v", err)
	}

	// (node_id, branch) is the nodes PK so the two repos must use distinct
	// node_ids; the test still proves the SELECT scopes by repo_id because
	// the repo2 node would otherwise match by file_path + branch.
	f.insertNode(t, "n-r1", f.branch, "pkg/a.go", "h-current-1")
	if _, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"n-r2", f.branch, "repo2", "go", "function", "n-r2", "pkg/a.go",
		1, 10, "h-current-2", time.Now().UnixMilli(), "service:veska", "system",
	); err != nil {
		t.Fatalf("insert node repo2: %v", err)
	}

	// Finding in repo1.
	_ = f.insertFinding(t, "u-r1", f.branch, "n-r1", new("h-old-1"))
	// Finding in repo2 — emulate via direct insert because the helper is keyed on repo1.
	fndR2, err := domain.NewFinding(
		"u-r2", "repo2", f.branch,
		domain.SeverityLow, domain.LayerStructural, "dead-code", "msg",
		domain.WithNodeAnchor("n-r2"), domain.WithAnchorContentHash("h-old-2"),
	)
	if err != nil {
		t.Fatalf("NewFinding r2: %v", err)
	}
	if err := f.findRepo.Save(context.Background(), fndR2); err != nil {
		t.Fatalf("Save r2: %v", err)
	}

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 1 || got[0].CurrentHash != "h-current-1" {
		t.Fatalf("got %+v, want exactly 1 row from repo1 (h-current-1)", got)
	}
}

func TestRevalidateRepo_Close_FlipsStateAndSetsActor(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-old"))

	closedAt := time.Now().UnixMilli()
	if err := f.reval.CloseAsRevalidatedObsolete(context.Background(), f.repoID, f.branch, fnd.FindingID, closedAt); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var (
		state, reason, actorID, actorKind string
		gotClosedAt                       sql.NullInt64
	)
	if err := f.db.QueryRow(
		`SELECT state, closed_reason, actor_id, actor_kind, closed_at FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&state, &reason, &actorID, &actorKind, &gotClosedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "closed" {
		t.Errorf("state = %q, want closed", state)
	}
	if reason != "revalidated_obsolete" {
		t.Errorf("reason = %q, want revalidated_obsolete", reason)
	}
	if actorID != "service:veska" {
		t.Errorf("actor_id = %q, want service:veska", actorID)
	}
	if actorKind != "system" {
		t.Errorf("actor_kind = %q, want system", actorKind)
	}
	if !gotClosedAt.Valid || gotClosedAt.Int64 != closedAt {
		t.Errorf("closed_at = %+v, want %d", gotClosedAt, closedAt)
	}
}

func TestRevalidateRepo_StaleFindings_CarriesRule(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-a", f.branch, "pkg/a.go", "h-current")
	_ = f.insertFinding(t, "u-a", f.branch, "n-a", new("h-old"))

	got, err := f.reval.StaleFindingsForFile(context.Background(), f.repoID, f.branch, "pkg/a.go")
	if err != nil {
		t.Fatalf("StaleFindingsForFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Rule != "dead-code" {
		t.Errorf("Rule = %q, want dead-code", got[0].Rule)
	}
}

func TestRevalidateRepo_HasInboundEdges(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-src", f.branch, "pkg/a.go", "h-a")
	f.insertNode(t, "n-dst-with", f.branch, "pkg/b.go", "h-b")
	f.insertNode(t, "n-dst-without", f.branch, "pkg/c.go", "h-c")

	if _, err := f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"edge-1", f.branch, f.repoID, "n-src", "n-dst-with", "call", "definite", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	got, err := f.reval.HasInboundEdges(context.Background(), f.repoID, f.branch, "n-dst-with")
	if err != nil {
		t.Fatalf("HasInboundEdges: %v", err)
	}
	if !got {
		t.Errorf("HasInboundEdges(n-dst-with) = false, want true")
	}

	got, err = f.reval.HasInboundEdges(context.Background(), f.repoID, f.branch, "n-dst-without")
	if err != nil {
		t.Fatalf("HasInboundEdges: %v", err)
	}
	if got {
		t.Errorf("HasInboundEdges(n-dst-without) = true, want false")
	}
}

func TestRevalidateRepo_HasInboundEdges_BranchScoped(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-src", "main", "pkg/a.go", "h-a")
	f.insertNode(t, "n-dst", "main", "pkg/b.go", "h-b")
	f.insertNode(t, "n-src", "feature", "pkg/a.go", "h-a-f")
	f.insertNode(t, "n-dst", "feature", "pkg/b.go", "h-b-f")
	// Edge only on feature branch.
	if _, err := f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"edge-feat", "feature", f.repoID, "n-src", "n-dst", "call", "definite", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	// main branch must see no inbound edge.
	got, err := f.reval.HasInboundEdges(context.Background(), f.repoID, "main", "n-dst")
	if err != nil {
		t.Fatalf("HasInboundEdges: %v", err)
	}
	if got {
		t.Errorf("HasInboundEdges(main, n-dst) = true, want false (feature-branch edge must not leak)")
	}
}

func TestRevalidateRepo_NodeSignaturePair(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	// Node with both signatures via direct insert.
	if _, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
        signature, prev_signature
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"n-sig", f.branch, f.repoID, "go", "function", "n-sig", "pkg/a.go",
		1, 10, "h-cur", time.Now().UnixMilli(), "service:veska", "system",
		"sig-current", "sig-prev",
	); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	prev, cur, err := f.reval.NodeSignaturePair(context.Background(), f.repoID, f.branch, "n-sig")
	if err != nil {
		t.Fatalf("NodeSignaturePair: %v", err)
	}
	if prev != "sig-prev" || cur != "sig-current" {
		t.Errorf("NodeSignaturePair = (%q, %q), want (sig-prev, sig-current)", prev, cur)
	}
}

func TestRevalidateRepo_NodeSignaturePair_NullsAndMissing(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)

	// Node with NULL signatures (e.g. field / file kind).
	f.insertNode(t, "n-null", f.branch, "pkg/a.go", "h-a")
	prev, cur, err := f.reval.NodeSignaturePair(context.Background(), f.repoID, f.branch, "n-null")
	if err != nil {
		t.Fatalf("NodeSignaturePair: %v", err)
	}
	if prev != "" || cur != "" {
		t.Errorf("NULL-signature node = (%q, %q), want both empty", prev, cur)
	}

	// Missing node.
	prev, cur, err = f.reval.NodeSignaturePair(context.Background(), f.repoID, f.branch, "n-absent")
	if err != nil {
		t.Fatalf("NodeSignaturePair missing: %v", err)
	}
	if prev != "" || cur != "" {
		t.Errorf("absent node = (%q, %q), want both empty", prev, cur)
	}
}

func TestRevalidateRepo_RefreshAnchorHash_RoundTrip(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-A"))

	// Confirm starting state.
	var anchor sql.NullString
	if err := f.db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&anchor); err != nil {
		t.Fatalf("query before: %v", err)
	}
	if !anchor.Valid || anchor.String != "h-A" {
		t.Fatalf("starting anchor = %+v, want h-A", anchor)
	}

	if err := f.reval.RefreshAnchorHash(context.Background(), f.repoID, f.branch, fnd.FindingID, "h-B", time.Now().UnixMilli()); err != nil {
		t.Fatalf("RefreshAnchorHash: %v", err)
	}

	var state string
	var newAnchor sql.NullString
	var reason sql.NullString
	if err := f.db.QueryRow(
		`SELECT state, anchor_content_hash, closed_reason FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&state, &newAnchor, &reason); err != nil {
		t.Fatalf("query after: %v", err)
	}
	if state != "open" {
		t.Errorf("state = %q, want open", state)
	}
	if !newAnchor.Valid || newAnchor.String != "h-B" {
		t.Errorf("anchor = %+v, want h-B", newAnchor)
	}
	if reason.Valid {
		t.Errorf("closed_reason = %q, want NULL", reason.String)
	}
}

func TestRevalidateRepo_RefreshAnchorHash_DoesNotResurrectClosed(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-A"))

	// Force-close it.
	if _, err := f.db.Exec(
		`UPDATE findings SET state='closed', closed_reason='manual', closed_at=? WHERE finding_id=? AND branch=?`,
		time.Now().UnixMilli(), fnd.FindingID, fnd.Branch,
	); err != nil {
		t.Fatalf("force-close: %v", err)
	}

	// Refresh must be a no-op (state='open' gate).
	if err := f.reval.RefreshAnchorHash(context.Background(), f.repoID, f.branch, fnd.FindingID, "h-B", time.Now().UnixMilli()); err != nil {
		t.Fatalf("RefreshAnchorHash: %v", err)
	}

	var state string
	var anchor sql.NullString
	if err := f.db.QueryRow(
		`SELECT state, anchor_content_hash FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&state, &anchor); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "closed" {
		t.Errorf("state = %q, want still closed", state)
	}
	if anchor.String != "h-A" {
		t.Errorf("anchor = %q, want unchanged h-A", anchor.String)
	}
}

func TestRevalidateRepo_Close_IsIdempotentOnAlreadyClosed(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-current")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-old"))

	t1 := time.Now().UnixMilli()
	if err := f.reval.CloseAsRevalidatedObsolete(context.Background(), f.repoID, f.branch, fnd.FindingID, t1); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second call must not error and must not re-stamp closed_at (state='open' gate).
	t2 := t1 + 9999
	if err := f.reval.CloseAsRevalidatedObsolete(context.Background(), f.repoID, f.branch, fnd.FindingID, t2); err != nil {
		t.Fatalf("second close: %v", err)
	}

	var gotClosedAt sql.NullInt64
	if err := f.db.QueryRow(
		`SELECT closed_at FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&gotClosedAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !gotClosedAt.Valid || gotClosedAt.Int64 != t1 {
		t.Errorf("closed_at = %+v, want unchanged %d", gotClosedAt, t1)
	}
}
