package autolink_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
)

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeLookup struct {
	byPath map[string][]string
	err    error
	calls  int
}

func (f *fakeLookup) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byPath[filePath], nil
}

type fakeLinker struct {
	out   []autolink.Candidate
	err   error
	calls int
}

func (f *fakeLinker) Candidates(_ context.Context, _, _ string, _ []string) ([]autolink.Candidate, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

type fakeEdgeStore struct {
	saved [][]*domain.Edge
	err   error
}

func (f *fakeEdgeStore) SaveEdges(_ context.Context, _, _ string, edges []*domain.Edge) error {
	if f.err != nil {
		return f.err
	}
	// copy so test mutations don't affect captured state
	cp := make([]*domain.Edge, len(edges))
	copy(cp, edges)
	f.saved = append(f.saved, cp)
	return nil
}

type fakeFindingStore struct {
	saved []*domain.Finding
	err   error
}

func (f *fakeFindingStore) Save(_ context.Context, fnd *domain.Finding) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, fnd)
	return nil
}

// ── unit-level tests against fakes ─────────────────────────────────────────

func TestHandler_RejectsWrongKind(t *testing.T) {
	t.Parallel()
	h := autolink.NewHandler(&fakeLinker{}, &fakeLookup{}, &fakeEdgeStore{}, &fakeFindingStore{})
	err := h.Handle(context.Background(), queue.Row{Kind: queue.WorkKindEmbed, Payload: "x.go"})
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

func TestHandler_EmptyPayloadIsNoop(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{}
	h := autolink.NewHandler(&fakeLinker{}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	err := h.Handle(context.Background(), queue.Row{Kind: queue.WorkKindAutoLink, Payload: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lk.calls != 0 {
		t.Errorf("expected zero lookup calls for empty payload, got %d", lk.calls)
	}
}

func TestHandler_FileWithZeroNodesIsNoop(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{byPath: map[string][]string{}}
	linker := &fakeLinker{}
	edges := &fakeEdgeStore{}
	findings := &fakeFindingStore{}
	h := autolink.NewHandler(linker, lk, edges, findings)

	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if linker.calls != 0 {
		t.Errorf("linker should not be called when file has zero nodes (got %d)", linker.calls)
	}
	if len(edges.saved) != 0 || len(findings.saved) != 0 {
		t.Errorf("expected no writes; edges=%v findings=%v", edges.saved, findings.saved)
	}
}

func TestHandler_NoCandidatesIsNoop(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{byPath: map[string][]string{"x.go": {"n1"}}}
	linker := &fakeLinker{out: nil}
	edges := &fakeEdgeStore{}
	findings := &fakeFindingStore{}
	h := autolink.NewHandler(linker, lk, edges, findings)

	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(edges.saved) != 0 || len(findings.saved) != 0 {
		t.Errorf("expected no writes; edges=%v findings=%v", edges.saved, findings.saved)
	}
}

func TestHandler_LookupErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-lookup")
	lk := &fakeLookup{err: sentinel}
	h := autolink.NewHandler(&fakeLinker{}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_LinkerErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-linker")
	lk := &fakeLookup{byPath: map[string][]string{"x.go": {"n1"}}}
	h := autolink.NewHandler(&fakeLinker{err: sentinel}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_EdgeStorageErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-edges")
	lk := &fakeLookup{byPath: map[string][]string{"x.go": {"n1"}}}
	linker := &fakeLinker{out: []autolink.Candidate{{SourceNodeID: "n1", TargetNodeID: "n2", Score: 0.9}}}
	h := autolink.NewHandler(linker, lk, &fakeEdgeStore{err: sentinel}, &fakeFindingStore{})
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_FindingStorageErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-findings")
	lk := &fakeLookup{byPath: map[string][]string{"x.go": {"n1"}}}
	linker := &fakeLinker{out: []autolink.Candidate{{SourceNodeID: "n1", TargetNodeID: "n2", Score: 0.9}}}
	h := autolink.NewHandler(linker, lk, &fakeEdgeStore{}, &fakeFindingStore{err: sentinel})
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_FakesEmitOneEdgeAndOneFindingPerCandidate(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{byPath: map[string][]string{"x.go": {"n1", "n2"}}}
	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "n1", TargetNodeID: "t1", Score: 0.91},
		{SourceNodeID: "n1", TargetNodeID: "t2", Score: 0.88},
		{SourceNodeID: "n2", TargetNodeID: "t3", Score: 0.95},
	}}
	edges := &fakeEdgeStore{}
	findings := &fakeFindingStore{}
	h := autolink.NewHandler(linker, lk, edges, findings)

	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(edges.saved) != 1 || len(edges.saved[0]) != 3 {
		t.Fatalf("expected single 3-edge batch, got %v", edges.saved)
	}
	if len(findings.saved) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(findings.saved))
	}
	// Each finding's node_id anchor must equal the corresponding edge's ID.
	saved := edges.saved[0]
	wantAnchors := map[string]bool{saved[0].ID: true, saved[1].ID: true, saved[2].ID: true}
	for _, f := range findings.saved {
		if f.NodeID == nil {
			t.Fatalf("finding missing node anchor: %+v", f)
		}
		if !wantAnchors[*f.NodeID] {
			t.Errorf("finding anchor %q does not match any edge ID", *f.NodeID)
		}
		if f.SourceLayer != domain.LayerSemantic {
			t.Errorf("SourceLayer = %v, want semantic", f.SourceLayer)
		}
		if f.Rule != autolink.Rule {
			t.Errorf("Rule = %q, want %q", f.Rule, autolink.Rule)
		}
		if f.Severity != domain.SeverityLow {
			t.Errorf("Severity = %v, want low", f.Severity)
		}
	}
}

// ── integration test against real SQLite adapters and a fake Linker ────────

// openHandlerIntegrationDB seeds a repo + nodes in (r1, main) and returns
// the live adapters the integration test wires into NewHandler.
func openHandlerIntegrationDB(t *testing.T) (
	*sql.DB, *sqlite.NodeLookupRepo, *sqlite.EdgeRepo, *sqlite.FindingRepo,
) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	for _, n := range []struct{ id, path string }{
		{"n1", "x.go"},
		{"n2", "x.go"},
		{"t1", "other.go"},
		{"t2", "other.go"},
		{"t3", "other.go"},
	} {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.id, "main", "r1", "go", "function", n.id, n.path,
			1, 10, "h-"+n.id, now, "service:veska", "system"); err != nil {
			t.Fatalf("insert node %s: %v", n.id, err)
		}
	}

	return db, sqlite.NewNodeLookupRepo(db), sqlite.NewEdgeRepo(db), sqlite.NewFindingRepo(db)
}

func TestHandler_Integration_PersistsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	rawDB, lookupRepo, edgeRepo, findingRepo := openHandlerIntegrationDB(t)

	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "n1", TargetNodeID: "t1", Score: 0.91},
		{SourceNodeID: "n1", TargetNodeID: "t2", Score: 0.88},
		{SourceNodeID: "n2", TargetNodeID: "t3", Score: 0.95},
	}}
	h := autolink.NewHandler(linker, lookupRepo, edgeRepo, findingRepo)

	row := queue.Row{Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go"}
	if err := h.Handle(context.Background(), row); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Re-run: must not duplicate rows (ON CONFLICT DO NOTHING / DO UPDATE).
	if err := h.Handle(context.Background(), row); err != nil {
		t.Fatalf("Handle (re-run): %v", err)
	}

	// Verify edges.
	var edgeCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM edges WHERE repo_id='r1' AND branch='main' AND kind='SIMILAR_TO' AND confidence='unresolved'`,
	).Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount != 3 {
		t.Errorf("expected 3 SIMILAR_TO unresolved edges, got %d", edgeCount)
	}

	// Verify findings.
	var findingCount int
	if err := rawDB.QueryRow(
		`SELECT COUNT(*) FROM findings WHERE repo_id='r1' AND branch='main' AND source_layer='semantic' AND rule='auto-link'`,
	).Scan(&findingCount); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if findingCount != 3 {
		t.Errorf("expected 3 auto-link findings, got %d", findingCount)
	}

	// Verify each finding anchors on an existing edge_id.
	rows, err := rawDB.Query(
		`SELECT node_id FROM findings WHERE repo_id='r1' AND branch='main' AND rule='auto-link' ORDER BY node_id`,
	)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	defer rows.Close()
	var anchors []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		anchors = append(anchors, a)
	}
	sort.Strings(anchors)

	edgeRows, err := rawDB.Query(
		`SELECT edge_id FROM edges WHERE repo_id='r1' AND branch='main' AND kind='SIMILAR_TO' ORDER BY edge_id`,
	)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer edgeRows.Close()
	var edgeIDs []string
	for edgeRows.Next() {
		var id string
		if err := edgeRows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		edgeIDs = append(edgeIDs, id)
	}
	sort.Strings(edgeIDs)

	if len(anchors) != len(edgeIDs) {
		t.Fatalf("anchor/edge id count mismatch: %d vs %d", len(anchors), len(edgeIDs))
	}
	for i := range anchors {
		if anchors[i] != edgeIDs[i] {
			t.Errorf("anchor[%d]=%q != edge_id[%d]=%q", i, anchors[i], i, edgeIDs[i])
		}
	}
}
