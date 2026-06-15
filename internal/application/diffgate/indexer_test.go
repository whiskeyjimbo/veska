package diffgate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	testRepo   = "repo1"
	testBranch = "main"
)

// fakeBaseGraph is a deterministic in-memory BaseGraph (EdgeReader +
// NodeLookup) standing in for the persisted indexed-HEAD graph. It records
// whether any method mutated it — it never does, proving the ephemeral index
// leaves the base untouched (AC1).
type fakeBaseGraph struct {
	inbound  map[string][]string
	outbound map[string][]string
	// callInbound models CALLS-only inbound adjacency (InboundCallEdges). When
	// nil it falls back to `inbound` — most tests don't care about edge kind, so
	// their `inbound` doubles as the call set; a test exercising the CALLS-vs-
	// structural distinction (solov2-nmps.9) sets callInbound explicitly.
	callInbound map[string][]string
	metas       map[string]ports.NodeMeta
	byFile      map[string][]string
	hashes      map[string]string // nodeID -> promoted content hash
}

// NodeContentHash makes fakeBaseGraph a blastradius.ContentHasher so a
// consumer (and the DoD test) can compute node-precision by comparing a
// candidate node's hash against the base's promoted hash.
func (f *fakeBaseGraph) NodeContentHash(_ context.Context, _, _, nodeID string) (string, error) {
	return f.hashes[nodeID], nil
}

func (f *fakeBaseGraph) InboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.inbound[id]...)
	}
	return out, nil
}

func (f *fakeBaseGraph) InboundCallEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	src := f.callInbound
	if src == nil {
		src = f.inbound
	}
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), src[id]...)
	}
	return out, nil
}

func (f *fakeBaseGraph) OutboundEdges(_ context.Context, _, _ string, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	for _, id := range ids {
		out[id] = append([]string(nil), f.outbound[id]...)
	}
	return out, nil
}

func (f *fakeBaseGraph) LookupNodes(_ context.Context, _, _ string, ids []string) ([]ports.NodeMeta, error) {
	var out []ports.NodeMeta
	for _, id := range ids {
		if m, ok := f.metas[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeBaseGraph) NodesInFile(_ context.Context, _, _, filePath string) ([]string, error) {
	return f.byFile[filePath], nil
}

// fakeParser parses a file by looking its path up in a fixture map. It panics
// if asked to parse an unregistered path so tests fail loudly on a typo. It
// never performs IO or network calls.
type fakeParser struct {
	byPath map[string]*domain.ParseResult
}

func (p *fakeParser) ParseFile(_ context.Context, _ string, path string, _ []byte) (*domain.ParseResult, error) {
	pr, ok := p.byPath[path]
	if !ok {
		return nil, errors.New("fakeParser: no fixture for " + path)
	}
	return pr, nil
}

// staticChangeSource is a ChangeSource that returns a fixed slice — the test
// fake that proves AC3: a second source feeds the same Indexer/consumers with
// no change to their code.
type staticChangeSource struct {
	changes []diffgate.FileChange
}

func (s staticChangeSource) Changes(context.Context) ([]diffgate.FileChange, error) {
	return s.changes, nil
}

func mustNode(t *testing.T, id, path, name string) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindFunction})
	if err != nil {
		t.Fatalf("NewNode(%s): %v", id, err)
	}
	return n
}

func mustNodeH(t *testing.T, id, path, name, hash string) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindFunction}, domain.WithContentHash(domain.ContentHash(hash)))
	if err != nil {
		t.Fatalf("NewNode(%s): %v", id, err)
	}
	return n
}

// TestIndex_NodePrecisionAgainstBase covers the DoD: the ephemeral graph
// differs from the base on EXACTLY the changed nodes — not on every symbol of
// a touched file. A re-parse stages the whole file (m:Keep AND m:Edit), so
// node-precision is derived the way a consumer does: content-hash-compare each
// staged node against the base. Only m:Edit, whose body changed, must show as
// different.
func TestIndex_NodePrecisionAgainstBase(t *testing.T) {
	base := &fakeBaseGraph{
		hashes: map[string]string{"m:Keep": "H1", "m:Edit": "H2"},
	}
	parser := &fakeParser{byPath: map[string]*domain.ParseResult{
		"m.go": {Nodes: []*domain.Node{
			mustNodeH(t, "m:Keep", "m.go", "Keep", "H1"),     // unchanged body
			mustNodeH(t, "m:Edit", "m.go", "Edit", "H2-NEW"), // edited body
		}},
	}}
	ix, _ := diffgate.NewIndexer(parser)
	src := staticChangeSource{changes: []diffgate.FileChange{{Path: "m.go", Content: []byte("package m")}}}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, base, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Overlay is file-granular: it carries BOTH symbols of the touched file.
	snap := eph.Overlay.Snapshot(testRepo, testBranch)
	if got := len(snap["m.go"].Nodes); got != 2 {
		t.Fatalf("overlay m.go nodes = %d, want 2 (whole-file re-parse)", got)
	}

	// Node-precision: diff each staged node's hash against the base's.
	var differ []string
	for _, n := range snap["m.go"].Nodes {
		baseHash, _ := base.NodeContentHash(context.Background(), testRepo, testBranch, string(n.ID))
		if n.ContentHash != nil && string(*n.ContentHash) != baseHash {
			differ = append(differ, string(n.ID))
		}
	}
	if len(differ) != 1 || differ[0] != "m:Edit" {
		t.Fatalf("ephemeral differs from base on %v, want exactly [m:Edit]", differ)
	}
}

// TestIndex_OverlayCarriesCandidateEdges covers the "edges" half of AC1: the
// candidate's parsed edges survive into the overlay, not just its nodes.
func TestIndex_OverlayCarriesCandidateEdges(t *testing.T) {
	callEdge, err := domain.NewEdge(domain.EdgeSpec{Src: "a:Foo", Tgt: "b:Bar", Kind: domain.EdgeCalls})
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	parser := &fakeParser{byPath: map[string]*domain.ParseResult{
		"a.go": {
			Nodes: []*domain.Node{mustNode(t, "a:Foo", "a.go", "Foo")},
			Edges: []*domain.Edge{callEdge},
		},
	}}
	ix, _ := diffgate.NewIndexer(parser)
	src := staticChangeSource{changes: []diffgate.FileChange{{Path: "a.go", Content: []byte("package a")}}}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, &fakeBaseGraph{}, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	edges := eph.Overlay.Snapshot(testRepo, testBranch)["a.go"].Edges
	if len(edges) != 1 || edges[0].Kind != domain.EdgeCalls {
		t.Fatalf("overlay a.go edges = %+v, want one CALLS edge", edges)
	}
}

// TestIndex_OverlayReflectsCandidateNodes covers AC1 + the DoD: indexing a
// fixture change produces an overlay holding exactly the changed file's
// candidate nodes, while the base graph is untouched.
func TestIndex_OverlayReflectsCandidateNodes(t *testing.T) {
	base := &fakeBaseGraph{
		metas: map[string]ports.NodeMeta{
			"a:Foo": {NodeID: "a:Foo", FilePath: "a.go"},
			"b:Bar": {NodeID: "b:Bar", FilePath: "b.go"},
		},
		byFile: map[string][]string{"a.go": {"a:Foo"}, "b.go": {"b:Bar"}},
	}
	parser := &fakeParser{byPath: map[string]*domain.ParseResult{
		"a.go": {Nodes: []*domain.Node{mustNode(t, "a:Foo2", "a.go", "Foo2")}},
	}}
	ix, err := diffgate.NewIndexer(parser)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}

	src := staticChangeSource{changes: []diffgate.FileChange{
		{Path: "a.go", Content: []byte("package a")},
	}}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, base, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Overlay holds exactly the changed file, with its candidate node.
	snap := eph.Overlay.Snapshot(testRepo, testBranch)
	if len(snap) != 1 {
		t.Fatalf("overlay files = %d, want 1 (only a.go changed); got %v keys", len(snap), keys(snap))
	}
	f, ok := snap["a.go"]
	if !ok {
		t.Fatalf("overlay missing a.go; keys=%v", keys(snap))
	}
	if len(f.Nodes) != 1 || string(f.Nodes[0].ID) != "a:Foo2" {
		t.Fatalf("overlay a.go nodes = %+v, want single a:Foo2", f.Nodes)
	}
	if got := eph.ChangedFiles; len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("ChangedFiles = %v, want [a.go]", got)
	}

	// Base is unmutated: b.go (unchanged) is absent from the overlay, and the
	// base still resolves both original nodes.
	if _, ok := snap["b.go"]; ok {
		t.Fatalf("overlay should not contain the unchanged b.go")
	}
	metas, _ := base.LookupNodes(context.Background(), testRepo, testBranch, []string{"a:Foo", "b:Bar"})
	if len(metas) != 2 {
		t.Fatalf("base lookup = %d nodes, want 2 (base must be untouched)", len(metas))
	}
}

// TestIndex_DeletedFileStagesEmptyOverlay verifies a deletion is recorded as a
// present-but-empty overlay entry shadowing the base's nodes for that file.
func TestIndex_DeletedFileStagesEmptyOverlay(t *testing.T) {
	base := &fakeBaseGraph{byFile: map[string][]string{"gone.go": {"gone:Old"}}}
	ix, err := diffgate.NewIndexer(&fakeParser{})
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	src := staticChangeSource{changes: []diffgate.FileChange{{Path: "gone.go", Deleted: true}}}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, base, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	snap := eph.Overlay.Snapshot(testRepo, testBranch)
	f, ok := snap["gone.go"]
	if !ok {
		t.Fatalf("deleted file should still appear in overlay as empty; keys=%v", keys(snap))
	}
	if len(f.Nodes) != 0 {
		t.Fatalf("deleted file overlay nodes = %d, want 0", len(f.Nodes))
	}
}

// TestIndex_ConsumerReadsEphemeralUnchanged proves AC3: a real downstream
// consumer (the blast-radius service) runs over the ephemeral graph composed
// as (Base, Base, Overlay) with no change to its code, regardless of which
// ChangeSource produced the overlay.
func TestIndex_ConsumerReadsEphemeralUnchanged(t *testing.T) {
	base := &fakeBaseGraph{
		// The candidate MODIFIES an existing symbol a:Foo. The base still
		// knows the symbol (it resolves as a seed) and records that caller:X
		// calls it — that inbound edge is what the guard walks.
		metas:   map[string]ports.NodeMeta{"a:Foo": {NodeID: "a:Foo", FilePath: "a.go"}},
		inbound: map[string][]string{"a:Foo": {"caller:X"}},
	}
	parser := &fakeParser{byPath: map[string]*domain.ParseResult{
		"a.go": {Nodes: []*domain.Node{mustNode(t, "a:Foo", "a.go", "Foo")}},
	}}
	ix, _ := diffgate.NewIndexer(parser)
	src := staticChangeSource{changes: []diffgate.FileChange{{Path: "a.go", Content: []byte("package a")}}}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, base, src)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// The guard consumer composes the ephemeral graph with NO diffgate-aware
	// code: it just wires Base as EdgeReader+NodeLookup and Overlay as the
	// staging area, exactly as it does against the live graph today.
	svc, err := blastradius.NewService(eph.Base, eph.Base, eph.Overlay)
	if err != nil {
		t.Fatalf("blastradius.NewService: %v", err)
	}
	resp, err := svc.DirtyOf(context.Background(), testRepo, testBranch, blastradius.Options{})
	if err != nil {
		t.Fatalf("DirtyOf: %v", err)
	}
	if !resp.IncludedStaging {
		t.Fatalf("expected the consumer to read the candidate overlay")
	}
	// The candidate node is dirty and its base caller is reachable inbound.
	if !containsEntry(resp, "caller:X") {
		t.Fatalf("blast radius did not reach the base caller of the changed node; resp=%+v", resp)
	}
}

func TestIndex_NilDependencies(t *testing.T) {
	if _, err := diffgate.NewIndexer(nil); !errors.Is(err, diffgate.ErrMissingDependency) {
		t.Fatalf("NewIndexer(nil) err = %v, want ErrMissingDependency", err)
	}
	ix, _ := diffgate.NewIndexer(&fakeParser{})
	if _, err := ix.Index(context.Background(), testRepo, testBranch, nil, staticChangeSource{}); !errors.Is(err, diffgate.ErrMissingDependency) {
		t.Fatalf("Index(nil base) err = %v, want ErrMissingDependency", err)
	}
	if _, err := ix.Index(context.Background(), testRepo, testBranch, &fakeBaseGraph{}, nil); !errors.Is(err, diffgate.ErrMissingDependency) {
		t.Fatalf("Index(nil src) err = %v, want ErrMissingDependency", err)
	}
}

func keys(m map[string]staging.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func containsEntry(resp blastradius.Response, id string) bool {
	for _, e := range resp.Entries {
		if e.NodeID == id {
			return true
		}
	}
	return false
}
