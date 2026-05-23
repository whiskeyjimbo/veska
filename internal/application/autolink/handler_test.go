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
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
)

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeLookup struct {
	byPath     map[string][]string
	contentBy  map[string]string
	meta       map[string]ports.NodeMeta
	err        error
	hashErr    error
	calls      int
	hashCalls  int
	gotHashIDs []string
}

func (f *fakeLookup) LookupNodes(_ context.Context, _, _ string, nodeIDs []string) ([]ports.NodeMeta, error) {
	if f.meta == nil {
		return nil, nil
	}
	out := make([]ports.NodeMeta, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if m, ok := f.meta[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeLookup) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byPath[filePath], nil
}

func (f *fakeLookup) NodeContentHash(_ context.Context, _, _, nodeID string) (string, error) {
	f.hashCalls++
	f.gotHashIDs = append(f.gotHashIDs, nodeID)
	if f.hashErr != nil {
		return "", f.hashErr
	}
	return f.contentBy[nodeID], nil
}

type fakeLinker struct {
	out        []autolink.Candidate
	err        error
	calls      int
	gotSources []string
}

func (f *fakeLinker) Candidates(_ context.Context, _, _ string, sources []string) ([]autolink.Candidate, error) {
	f.calls++
	f.gotSources = append(f.gotSources, sources...)
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

func (f *fakeFindingStore) CloseObsolete(_ context.Context, _, _ string) error {
	return f.err
}

// ── unit-level tests against fakes ─────────────────────────────────────────

// mustHandler unwraps an autolink.NewHandler result, failing the test if the
// constructor returned an error.
func mustHandler(t *testing.T, h *autolink.Handler, err error) *autolink.Handler {
	t.Helper()
	if err != nil {
		t.Fatalf("autolink.NewHandler: %v", err)
	}
	return h
}

func TestNewHandler_NilDependencyReturnsTypedError(t *testing.T) {
	linker := &fakeLinker{}
	lookup := &fakeLookup{}
	edges := &fakeEdgeStore{}
	findings := &fakeFindingStore{}

	cases := []struct {
		name                  string
		nilLinker, nilLookup  bool
		nilEdges, nilFindings bool
	}{
		{name: "nil linker", nilLinker: true},
		{name: "nil lookup", nilLookup: true},
		{name: "nil edges", nilEdges: true},
		{name: "nil findings", nilFindings: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				lk                       = linker
				lu                       = lookup
				ed  ports.EdgeStorage    = edges
				fnd ports.FindingStorage = findings
			)
			var h *autolink.Handler
			var err error
			switch {
			case tc.nilLinker:
				h, err = autolink.NewHandler(nil, lu, ed, fnd)
			case tc.nilLookup:
				h, err = autolink.NewHandler(lk, nil, ed, fnd)
			case tc.nilEdges:
				h, err = autolink.NewHandler(lk, lu, nil, fnd)
			case tc.nilFindings:
				h, err = autolink.NewHandler(lk, lu, ed, nil)
			}
			if h != nil {
				t.Errorf("expected nil *Handler, got %v", h)
			}
			if !errors.Is(err, autolink.ErrMissingDependency) {
				t.Fatalf("err = %v, want wraps ErrMissingDependency", err)
			}
		})
	}
}

func TestHandler_RejectsWrongKind(t *testing.T) {
	t.Parallel()
	hh, herr := autolink.NewHandler(&fakeLinker{}, &fakeLookup{}, &fakeEdgeStore{}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
	err := h.Handle(context.Background(), queue.Row{Kind: queue.WorkKindEmbed, Payload: "x.go"})
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
		return
	}
}

func TestHandler_EmptyPayloadIsNoop(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{}
	hh, herr := autolink.NewHandler(&fakeLinker{}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
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
	hh, herr := autolink.NewHandler(linker, lk, edges, findings)
	h := mustHandler(t, hh, herr)

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
	hh, herr := autolink.NewHandler(linker, lk, edges, findings)
	h := mustHandler(t, hh, herr)

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
	hh, herr := autolink.NewHandler(&fakeLinker{}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
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
	hh, herr := autolink.NewHandler(&fakeLinker{err: sentinel}, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
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
	hh, herr := autolink.NewHandler(linker, lk, &fakeEdgeStore{err: sentinel}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
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
	hh, herr := autolink.NewHandler(linker, lk, &fakeEdgeStore{}, &fakeFindingStore{err: sentinel})
	h := mustHandler(t, hh, herr)
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// TestHandler_SkipsNonSymbolSourcesAndNamesTarget covers solov2-wh0: package /
// chunk source nodes are dropped before linking, and the finding message names
// the target symbol + file instead of an opaque node ID.
func TestHandler_SkipsNonSymbolSourcesAndNamesTarget(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{
		byPath: map[string][]string{"x.go": {"fn1", "pkg1", "chunk1"}},
		meta: map[string]ports.NodeMeta{
			"fn1":    {NodeID: "fn1", Kind: "function", SymbolPath: "DoThing"},
			"pkg1":   {NodeID: "pkg1", Kind: "package", SymbolPath: "server"},
			"chunk1": {NodeID: "chunk1", Kind: "chunk", SymbolPath: "chunk:1-4"},
			"t1":     {NodeID: "t1", Kind: "function", SymbolPath: "OtherThing", FilePath: "y.go"},
		},
	}
	// The linker should only ever be asked about the symbol source (fn1).
	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "fn1", TargetNodeID: "t1", Score: 0.91},
	}}
	edges := &fakeEdgeStore{}
	findings := &fakeFindingStore{}
	hh, herr := autolink.NewHandler(linker, lk, edges, findings)
	h := mustHandler(t, hh, herr)

	if err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(linker.gotSources) != 1 || linker.gotSources[0] != "fn1" {
		t.Fatalf("linker sources = %v, want only [fn1] (package/chunk dropped)", linker.gotSources)
	}
	if len(findings.saved) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings.saved))
	}
	want := "Similar to OtherThing in y.go (score 0.91)"
	if findings.saved[0].Message != want {
		t.Errorf("message = %q, want %q", findings.saved[0].Message, want)
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
	hh, herr := autolink.NewHandler(linker, lk, edges, findings)
	h := mustHandler(t, hh, herr)

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
			return
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

// TestHandler_ThreadsSourceContentHashOntoFinding verifies the handler reads
// each source node's content_hash via the lookup and threads it onto the
// emitted finding. Source nodes shared across multiple candidates must only
// be looked up once (cache hit).
func TestHandler_ThreadsSourceContentHashOntoFinding(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{
		byPath:    map[string][]string{"x.go": {"n1", "n2"}},
		contentBy: map[string]string{"n1": "h-src1", "n2": "h-src2"},
	}
	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "n1", TargetNodeID: "t1", Score: 0.91},
		{SourceNodeID: "n1", TargetNodeID: "t2", Score: 0.88},
		{SourceNodeID: "n2", TargetNodeID: "t3", Score: 0.95},
	}}
	findings := &fakeFindingStore{}
	hh, herr := autolink.NewHandler(linker, lk, &fakeEdgeStore{}, findings)
	h := mustHandler(t, hh, herr)

	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(findings.saved) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(findings.saved))
	}
	wantBySrc := map[string]string{"n1": "h-src1", "n2": "h-src2"}
	for i, f := range findings.saved {
		src := linker.out[i].SourceNodeID
		if f.AnchorContentHash == nil {
			t.Errorf("finding[%d] (src=%s): AnchorContentHash nil", i, src)
			continue
		}
		if *f.AnchorContentHash != wantBySrc[src] {
			t.Errorf("finding[%d] (src=%s): AnchorContentHash=%q want %q",
				i, src, *f.AnchorContentHash, wantBySrc[src])
		}
	}
	// Two distinct source nodes => two distinct lookup calls (cache hit on the
	// repeated 'n1'). Order is insertion-driven by the candidate list.
	if lk.hashCalls != 2 {
		t.Errorf("NodeContentHash call count = %d, want 2 (cached re-use of n1)", lk.hashCalls)
	}
}

// TestHandler_MissingSourceHashStaysNil verifies that when the lookup returns
// "" (unknown source / no hash recorded) the finding's AnchorContentHash stays
// nil rather than being set to the empty string.
func TestHandler_MissingSourceHashStaysNil(t *testing.T) {
	t.Parallel()
	lk := &fakeLookup{
		byPath:    map[string][]string{"x.go": {"n1"}},
		contentBy: map[string]string{}, // no hash recorded
	}
	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "n1", TargetNodeID: "t1", Score: 0.9},
	}}
	findings := &fakeFindingStore{}
	hh, herr := autolink.NewHandler(linker, lk, &fakeEdgeStore{}, findings)
	h := mustHandler(t, hh, herr)

	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(findings.saved) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings.saved))
	}
	if findings.saved[0].AnchorContentHash != nil {
		t.Errorf("AnchorContentHash should be nil when source has no hash, got %q",
			*findings.saved[0].AnchorContentHash)
	}
}

// TestHandler_NodeContentHashErrorWraps verifies that a failure from the
// lookup's content-hash method aborts the row with a wrapped error so the
// queue.Poller can re-queue.
func TestHandler_NodeContentHashErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-hash")
	lk := &fakeLookup{
		byPath:  map[string][]string{"x.go": {"n1"}},
		hashErr: sentinel,
	}
	linker := &fakeLinker{out: []autolink.Candidate{
		{SourceNodeID: "n1", TargetNodeID: "t1", Score: 0.9},
	}}
	hh, herr := autolink.NewHandler(linker, lk, &fakeEdgeStore{}, &fakeFindingStore{})
	h := mustHandler(t, hh, herr)
	err := h.Handle(context.Background(), queue.Row{
		Kind: queue.WorkKindAutoLink, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
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
	hh, herr := autolink.NewHandler(linker, lookupRepo, edgeRepo, findingRepo)
	h := mustHandler(t, hh, herr)

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

	// Anchor content_hash must equal the source node's nodes.content_hash on
	// every auto-link finding. The integration fixture seeds source nodes with
	// content_hash = "h-<id>" so we can verify the threading end-to-end.
	hashRows, err := rawDB.Query(`SELECT anchor_content_hash FROM findings
		WHERE repo_id='r1' AND branch='main' AND rule='auto-link'`)
	if err != nil {
		t.Fatalf("query hashes: %v", err)
	}
	defer hashRows.Close()
	var seen int
	for hashRows.Next() {
		var got sql.NullString
		if err := hashRows.Scan(&got); err != nil {
			t.Fatalf("scan hash: %v", err)
		}
		if !got.Valid {
			t.Error("anchor_content_hash unexpectedly NULL")
			continue
		}
		// Sources are n1 (twice) and n2 (once); both have h-n1/h-n2 fixtures.
		if got.String != "h-n1" && got.String != "h-n2" {
			t.Errorf("unexpected anchor_content_hash %q", got.String)
		}
		seen++
	}
	if seen != 3 {
		t.Errorf("expected 3 hash rows, got %d", seen)
	}
}
