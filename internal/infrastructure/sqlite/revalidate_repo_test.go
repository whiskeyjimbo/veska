// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// revalFixture sets up a temporary database fixture for testing RevalidateRepo.
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
	fnd, err := domain.NewFinding(domain.FindingSpec{RepoID: f.repoID, Branch: branch, Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "msg"}, opts...,
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	if err := f.findRepo.Save(context.Background(), fnd); err != nil {
		t.Fatalf("Save finding: %v", err)
	}
	return fnd
}

func TestRevalidateRepo_StaleFindings_ReturnsOnlyDrift(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)

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
	f.insertNode(t, "n-x", "main", "pkg/a.go", "h-main")
	f.insertNode(t, "n-x", "feature", "pkg/a.go", "h-feature")

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
	if _, err := f.db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo2", "/tmp/repo2", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo2: %v", err)
	}

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

	_ = f.insertFinding(t, "u-r1", f.branch, "n-r1", new("h-old-1"))
	fndR2, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo2", Branch: f.branch, Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "msg"}, domain.WithNodeAnchor("n-r2"), domain.WithAnchorContentHash("h-old-2"))
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

func TestRevalidateRepo_HasInboundCallEdges(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-src", f.branch, "pkg/a.go", "h-a")
	f.insertNode(t, "n-pkg", f.branch, "pkg/p.go", "h-p")
	f.insertNode(t, "n-dst-with", f.branch, "pkg/b.go", "h-b")
	f.insertNode(t, "n-dst-without", f.branch, "pkg/c.go", "h-c")
	f.insertNode(t, "n-dst-contains", f.branch, "pkg/d.go", "h-d")

	mkEdge := func(id, src, dst, kind string) {
		if _, err := f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, f.branch, f.repoID, src, dst, kind, "definite", time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("insert edge %s: %v", id, err)
		}
	}
	mkEdge("edge-call", "n-src", "n-dst-with", "CALLS")
	mkEdge("edge-contains", "n-pkg", "n-dst-contains", "CONTAINS")

	cases := []struct {
		node string
		want bool
	}{
		{"n-dst-with", true},      // has a CALLS caller → live
		{"n-dst-without", false},  // no inbound at all → dead
		{"n-dst-contains", false}, // only a CONTAINS parent → still dead (nmps.9)
	}
	for _, c := range cases {
		got, err := f.reval.HasInboundCallEdges(context.Background(), f.repoID, f.branch, c.node)
		if err != nil {
			t.Fatalf("HasInboundCallEdges(%s): %v", c.node, err)
		}
		if got != c.want {
			t.Errorf("HasInboundCallEdges(%s) = %v, want %v", c.node, got, c.want)
		}
	}
}

func TestRevalidateRepo_HasTestCaller(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-testcaller", f.branch, "pkg/a_test.go", "h-t") // test-shaped src
	f.insertNode(t, "n-prodcaller", f.branch, "pkg/b.go", "h-p")      // non-test src
	f.insertNode(t, "n-tested", f.branch, "pkg/c.go", "h-c")          // has a test caller
	f.insertNode(t, "n-untested", f.branch, "pkg/d.go", "h-d")        // only a prod caller

	mkEdge := func(id, src, dst, kind string) {
		if _, err := f.db.Exec(`INSERT INTO edges (
            edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, f.branch, f.repoID, src, dst, kind, "definite", time.Now().UnixMilli(),
		); err != nil {
			t.Fatalf("insert edge %s: %v", id, err)
		}
	}
	mkEdge("e-test", "n-testcaller", "n-tested", "CALLS") // test file CALLS n-tested
	mkEdge("e-prod", "n-prodcaller", "n-untested", "CALLS")
	mkEdge("e-contains", "n-testcaller", "n-untested", "CONTAINS")

	got, err := f.reval.HasTestCaller(context.Background(), f.repoID, f.branch, "n-tested")
	if err != nil {
		t.Fatalf("HasTestCaller: %v", err)
	}
	if !got {
		t.Errorf("HasTestCaller(n-tested) = false, want true (CALLS from a _test.go src)")
	}

	got, err = f.reval.HasTestCaller(context.Background(), f.repoID, f.branch, "n-untested")
	if err != nil {
		t.Fatalf("HasTestCaller: %v", err)
	}
	if got {
		t.Errorf("HasTestCaller(n-untested) = true, want false (only a prod CALLS caller + a CONTAINS from test)")
	}
}

func TestRevalidateRepo_HasInboundCallEdges_BranchScoped(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-src", "main", "pkg/a.go", "h-a")
	f.insertNode(t, "n-dst", "main", "pkg/b.go", "h-b")
	f.insertNode(t, "n-src", "feature", "pkg/a.go", "h-a-f")
	f.insertNode(t, "n-dst", "feature", "pkg/b.go", "h-b-f")
	if _, err := f.db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"edge-feat", "feature", f.repoID, "n-src", "n-dst", "CALLS", "definite", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert edge: %v", err)
	}
	got, err := f.reval.HasInboundCallEdges(context.Background(), f.repoID, "main", "n-dst")
	if err != nil {
		t.Fatalf("HasInboundCallEdges: %v", err)
	}
	if got {
		t.Errorf("HasInboundCallEdges(main, n-dst) = true, want false (feature-branch edge must not leak)")
	}
}

func TestRevalidateRepo_NodeSignaturePair(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
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

	f.insertNode(t, "n-null", f.branch, "pkg/a.go", "h-a")
	prev, cur, err := f.reval.NodeSignaturePair(context.Background(), f.repoID, f.branch, "n-null")
	if err != nil {
		t.Fatalf("NodeSignaturePair: %v", err)
	}
	if prev != "" || cur != "" {
		t.Errorf("NULL-signature node = (%q, %q), want both empty", prev, cur)
	}

	prev, cur, err = f.reval.NodeSignaturePair(context.Background(), f.repoID, f.branch, "n-absent")
	if err != nil {
		t.Fatalf("NodeSignaturePair missing: %v", err)
	}
	if prev != "" || cur != "" {
		t.Errorf("absent node = (%q, %q), want both empty", prev, cur)
	}
}

func TestRevalidateRepo_ApplyDecisions_MixedBatch_RoundTrips(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)

	f.insertNode(t, "n-r1", f.branch, "pkg/a.go", "h-cur-1")
	f.insertNode(t, "n-r2", f.branch, "pkg/a.go", "h-cur-2")
	f.insertNode(t, "n-c1", f.branch, "pkg/a.go", "h-cur-3")
	f.insertNode(t, "n-c2", f.branch, "pkg/a.go", "h-cur-4")
	fR1 := f.insertFinding(t, "u-r1", f.branch, "n-r1", new("h-old-r1"))
	fR2 := f.insertFinding(t, "u-r2", f.branch, "n-r2", new("h-old-r2"))
	fC1 := f.insertFinding(t, "u-c1", f.branch, "n-c1", new("h-old-c1"))
	fC2 := f.insertFinding(t, "u-c2", f.branch, "n-c2", new("h-old-c2"))

	at := time.Now().UnixMilli()
	batch := []ports.FindingDecision{
		{FindingID: fR1.FindingID, Kind: ports.DecisionRefresh, NewHash: "h-new-r1"},
		{FindingID: fC1.FindingID, Kind: ports.DecisionClose},
		{FindingID: fR2.FindingID, Kind: ports.DecisionRefresh, NewHash: "h-new-r2"},
		{FindingID: fC2.FindingID, Kind: ports.DecisionClose},
	}
	if err := f.reval.ApplyDecisions(context.Background(), f.repoID, f.branch, batch, at); err != nil {
		t.Fatalf("ApplyDecisions: %v", err)
	}

	type row struct {
		state, reason, anchor string
		actorID, actorKind    string
		closedAt              sql.NullInt64
	}
	get := func(id string) row {
		t.Helper()
		var r row
		var reason, anchor, actorID, actorKind sql.NullString
		if err := f.db.QueryRow(
			`SELECT state, closed_reason, anchor_content_hash, actor_id, actor_kind, closed_at FROM findings WHERE finding_id=? AND branch=?`,
			id, f.branch,
		).Scan(&r.state, &reason, &anchor, &actorID, &actorKind, &r.closedAt); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if reason.Valid {
			r.reason = reason.String
		}
		if anchor.Valid {
			r.anchor = anchor.String
		}
		if actorID.Valid {
			r.actorID = actorID.String
		}
		if actorKind.Valid {
			r.actorKind = actorKind.String
		}
		return r
	}

	if r := get(fR1.FindingID); r.state != "open" || r.anchor != "h-new-r1" || r.reason != "" {
		t.Errorf("u-r1 = %+v, want open/h-new-r1/no-reason", r)
	}
	if r := get(fR2.FindingID); r.state != "open" || r.anchor != "h-new-r2" || r.reason != "" {
		t.Errorf("u-r2 = %+v, want open/h-new-r2/no-reason", r)
	}
	for _, id := range []string{fC1.FindingID, fC2.FindingID} {
		r := get(id)
		if r.state != "closed" || r.reason != "revalidated_obsolete" {
			t.Errorf("%s = %+v, want closed/revalidated_obsolete", id, r)
		}
		if r.actorID != "service:veska" || r.actorKind != "system" {
			t.Errorf("%s actor = %q/%q, want service:veska/system", id, r.actorID, r.actorKind)
		}
		if !r.closedAt.Valid || r.closedAt.Int64 != at {
			t.Errorf("%s closed_at = %+v, want %d", id, r.closedAt, at)
		}
	}
}

func TestRevalidateRepo_ApplyDecisions_EmptyBatchNoop(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-cur")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-old"))

	if err := f.reval.ApplyDecisions(context.Background(), f.repoID, f.branch, nil, time.Now().UnixMilli()); err != nil {
		t.Fatalf("ApplyDecisions(nil): %v", err)
	}
	var state string
	var anchor sql.NullString
	if err := f.db.QueryRow(
		`SELECT state, anchor_content_hash FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&state, &anchor); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "open" || anchor.String != "h-old" {
		t.Errorf("post no-op = state=%q anchor=%q, want open/h-old", state, anchor.String)
	}
}

func TestRevalidateRepo_ApplyDecisions_RollbackOnError(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-r", f.branch, "pkg/a.go", "h-cur-r")
	fR := f.insertFinding(t, "u-r", f.branch, "n-r", new("h-old-r"))

	batch := []ports.FindingDecision{
		{FindingID: fR.FindingID, Kind: ports.DecisionRefresh, NewHash: "h-new-r"},
		{FindingID: "bogus", Kind: ports.DecisionKind(99)},
	}
	if err := f.reval.ApplyDecisions(context.Background(), f.repoID, f.branch, batch, time.Now().UnixMilli()); err == nil {
		t.Fatal("expected error from unknown kind, got nil")
		return
	}

	var anchor sql.NullString
	if err := f.db.QueryRow(
		`SELECT anchor_content_hash FROM findings WHERE finding_id=? AND branch=?`,
		fR.FindingID, fR.Branch,
	).Scan(&anchor); err != nil {
		t.Fatalf("query: %v", err)
	}
	if anchor.String != "h-old-r" {
		t.Errorf("anchor after rollback = %q, want unchanged h-old-r", anchor.String)
	}
}

func TestRevalidateRepo_ApplyDecisions_GatedOnOpenState(t *testing.T) {
	t.Parallel()
	f := setupRevalFixture(t)
	f.insertNode(t, "n-x", f.branch, "pkg/a.go", "h-cur")
	fnd := f.insertFinding(t, "u-x", f.branch, "n-x", new("h-old"))

	if _, err := f.db.Exec(
		`UPDATE findings SET state='closed', closed_reason='manual', closed_at=? WHERE finding_id=? AND branch=?`,
		time.Now().UnixMilli(), fnd.FindingID, fnd.Branch,
	); err != nil {
		t.Fatalf("force-close: %v", err)
	}

	batch := []ports.FindingDecision{
		{FindingID: fnd.FindingID, Kind: ports.DecisionRefresh, NewHash: "h-new"},
	}
	if err := f.reval.ApplyDecisions(context.Background(), f.repoID, f.branch, batch, time.Now().UnixMilli()); err != nil {
		t.Fatalf("ApplyDecisions: %v", err)
	}
	var state string
	var anchor sql.NullString
	if err := f.db.QueryRow(
		`SELECT state, anchor_content_hash FROM findings WHERE finding_id=? AND branch=?`,
		fnd.FindingID, fnd.Branch,
	).Scan(&state, &anchor); err != nil {
		t.Fatalf("query: %v", err)
	}
	if state != "closed" || anchor.String != "h-old" {
		t.Errorf("post-apply on closed row = state=%q anchor=%q, want unchanged closed/h-old", state, anchor.String)
	}
}
