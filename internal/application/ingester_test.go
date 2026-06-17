package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// recordingFindingStorage captures every Save call and tracks the open/closed
// state of stored findings keyed by (finding_id, branch) so tests can assert
// CloseObsolete behaviour.
type recordingFindingStorage struct {
	mu       sync.Mutex
	findings []*domain.Finding
	closed   map[string]string // (finding_id|branch) -> closed_reason
	err      error
	closeErr error
}

func (r *recordingFindingStorage) Save(_ context.Context, f *domain.Finding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.findings = append(r.findings, f)
	return nil
}

// CloseObsolete flips the matching open finding to closed in the in-memory
// store, mirroring the SQLite adapter's no-op-on-no-match semantics.
func (r *recordingFindingStorage) CloseObsolete(_ context.Context, findingID, branch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closeErr != nil {
		return r.closeErr
	}
	for _, f := range r.findings {
		if f.FindingID == findingID && f.Branch == branch {
			if r.closed == nil {
				r.closed = make(map[string]string)
			}
			r.closed[findingID+"|"+branch] = "revalidated_obsolete"
		}
	}
	return nil
}

// CloseSupersededAutoLinks is a no-op for the ingester tests: the ingester
// does not produce auto-link findings.
func (r *recordingFindingStorage) CloseSupersededAutoLinks(_ context.Context, _, _ string, _ []string) error {
	return nil
}

func (r *recordingFindingStorage) CloseSupersededByRule(_ context.Context, _, _, _ string, _ []string) error {
	return nil
}

// closedReason returns the recorded close reason for a finding, or "" if the
// finding was never closed.
func (r *recordingFindingStorage) closedReason(findingID, branch string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed[findingID+"|"+branch]
}

func (r *recordingFindingStorage) snapshot() []*domain.Finding {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Finding, len(r.findings))
	copy(out, r.findings)
	return out
}

// stubParser is an in-test implementation of ports.CodeParser.
type stubParser struct {
	result *domain.ParseResult
	err    error
	called bool
}

func (s *stubParser) ParseFile(_ context.Context, _, _ string, _ []byte) (*domain.ParseResult, error) {
	s.called = true
	return s.result, s.err
}

func TestIngester_Save_StagesResult(t *testing.T) {
	nodes := []*domain.Node{{}}
	edges := []*domain.Edge{{}}
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: nodes, Edges: edges},
	}
	area := staging.NewArea()
	ing := NewIngester(parser, area, staging.NewGate(area))

	ing.Save(context.Background(), "repo1", "main", "foo/bar.go", []byte("package foo"))
	if !parser.called {
		t.Fatal("expected ParseFile to be called")
	}

	gotNodes, ok := area.GetStagedNodes("repo1", "main", "foo/bar.go")
	if !ok {
		t.Fatal("expected file to be staged after Save")
	}
	if len(gotNodes) != 1 {
		t.Fatalf("expected 1 staged node, got %d", len(gotNodes))
	}

	gotEdges, ok := area.GetStagedEdges("repo1", "main", "foo/bar.go")
	if !ok {
		t.Fatal("expected staged edges to be present")
	}
	if len(gotEdges) != 1 {
		t.Fatalf("expected 1 staged edge, got %d", len(gotEdges))
	}
}

func TestIngester_Save_ParseErrorIsNonFatal(t *testing.T) {
	parser := &stubParser{err: errors.New("syntax error")}
	area := staging.NewArea()
	ing := NewIngester(parser, area, staging.NewGate(area))

	ing.Save(context.Background(), "repo1", "main", "bad.go", []byte("not go"))

	_, staged := area.GetStagedNodes("repo1", "main", "bad.go")
	if staged {
		t.Fatal("file should not be staged when parse fails")
	}
}

func TestIngester_DeleteFile_RemovesFromStaging(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: []*domain.Node{{}}, Edges: nil},
	}
	area := staging.NewArea()
	ing := NewIngester(parser, area, staging.NewGate(area))

	ing.Save(context.Background(), "repo1", "main", "del.go", []byte("package x"))

	_, staged := area.GetStagedNodes("repo1", "main", "del.go")
	if !staged {
		t.Fatal("expected file to be staged before delete")
	}

	ing.DeleteFile("repo1", "main", "del.go")

	_, staged = area.GetStagedNodes("repo1", "main", "del.go")
	if staged {
		t.Fatal("expected file to be removed from staging after DeleteFile")
	}
}

func TestIngester_Save_ParseFailureEmitsFinding(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 3, Message: "syntax error"}},
		},
	}
	area := staging.NewArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, area, staging.NewGate(area), WithFindingStorage(store))

	ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken"))

	got := store.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	f := got[0]
	if f.Rule != "parse-failure" {
		t.Errorf("Rule = %q, want %q", f.Rule, "parse-failure")
	}
	if f.SourceLayer != domain.LayerStructural {
		t.Errorf("SourceLayer = %q, want structural", f.SourceLayer)
	}
	if f.FilePath == nil || *f.FilePath != "src/bad.ts" {
		t.Errorf("FilePath = %v, want pointer to %q", f.FilePath, "src/bad.ts")
	}
	if f.RepoID != "repo1" || f.Branch != "main" {
		t.Errorf("repo/branch = %q/%q, want repo1/main", f.RepoID, f.Branch)
	}
	if f.FindingID == "" {
		t.Error("expected non-empty FindingID")
	}
}

func TestIngester_Save_ParseFailureIdempotent(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 1, Message: "syntax error"}},
		},
	}
	area := staging.NewArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, area, staging.NewGate(area), WithFindingStorage(store))

	// Ingest the same broken file twice.
	for range 2 {
		ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken"))
	}

	got := store.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 Save calls, got %d", len(got))
	}
	// Idempotency means the same FindingID on every call - repo SQL layer
	// then collapses to one row via ON CONFLICT(finding_id, branch).
	if got[0].FindingID != got[1].FindingID {
		t.Errorf("FindingID not deterministic: %q vs %q", got[0].FindingID, got[1].FindingID)
	}
}

func TestIngester_Save_CleanParseEmitsNoFinding(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: []*domain.Node{{}}},
	}
	area := staging.NewArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, area, staging.NewGate(area), WithFindingStorage(store))

	ing.Save(context.Background(), "repo1", "main", "ok.go", []byte("package x"))
	if got := store.snapshot(); len(got) != 0 {
		t.Errorf("expected zero findings, got %d", len(got))
	}
}

func TestIngester_Save_CleanReparseClosesParseFailureFinding(t *testing.T) {
	area := staging.NewArea()
	store := &recordingFindingStorage{}
	parser := &stubParser{}
	ing := NewIngester(parser, area, staging.NewGate(area), WithFindingStorage(store))

	const path = "src/bad.ts"

	// First ingest: file fails to parse - a parse-failure finding opens.
	parser.result = &domain.ParseResult{
		Failures: []domain.ParseFailure{{Line: 3, Message: "syntax error"}},
	}
	ing.Save(context.Background(), "repo1", "main", path, []byte("broken"))

	saved := store.snapshot()
	if len(saved) != 1 {
		t.Fatalf("expected 1 finding after failing parse, got %d", len(saved))
	}
	fid := saved[0].FindingID
	if r := store.closedReason(fid, "main"); r != "" {
		t.Fatalf("finding closed prematurely: reason %q", r)
	}

	// Second ingest: same file now parses cleanly - the finding must close.
	parser.result = &domain.ParseResult{Nodes: []*domain.Node{{}}}
	ing.Save(context.Background(), "repo1", "main", path, []byte("package x"))

	if r := store.closedReason(fid, "main"); r != "revalidated_obsolete" {
		t.Errorf("parse-failure finding closed_reason = %q, want revalidated_obsolete", r)
	}
}

func TestIngester_Save_CleanParseNeverFailed_NoFindingCreated(t *testing.T) {
	area := staging.NewArea()
	store := &recordingFindingStorage{}
	parser := &stubParser{result: &domain.ParseResult{Nodes: []*domain.Node{{}}}}
	ing := NewIngester(parser, area, staging.NewGate(area), WithFindingStorage(store))

	ing.Save(context.Background(), "repo1", "main", "ok.go", []byte("package x"))

	if got := store.snapshot(); len(got) != 0 {
		t.Errorf("expected zero findings, got %d", len(got))
	}
}

func TestIngester_Save_ParseFailureWithoutFindingStorage_NoPanic(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 1, Message: "syntax error"}},
		},
	}
	area := staging.NewArea()
	ing := NewIngester(parser, area, staging.NewGate(area))
	// Deliberately omit WithFindingStorage.

	ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken"))
}

func TestIngester_Save_EmptyParseResultStagesFile(t *testing.T) {
	// Even an empty parse result should call StageFile (to clear old staged state).
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: nil, Edges: nil},
	}
	area := staging.NewArea()
	// Pre-seed staging with an old entry.
	area.Stage("repo1", "main", "empty.go", staging.File{Nodes: []*domain.Node{{}}, Edges: nil})

	ing := NewIngester(parser, area, staging.NewGate(area))
	ing.Save(context.Background(), "repo1", "main", "empty.go", []byte("package x"))

	gotNodes, ok := area.GetStagedNodes("repo1", "main", "empty.go")
	if !ok {
		t.Fatal("expected staging entry to exist after Save with empty result")
	}
	if len(gotNodes) != 0 {
		t.Fatalf("expected 0 staged nodes (empty result), got %d", len(gotNodes))
	}
}
