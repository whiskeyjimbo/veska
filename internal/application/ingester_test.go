package application

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// recordingFindingStorage captures every Save call.
type recordingFindingStorage struct {
	mu       sync.Mutex
	findings []*domain.Finding
	err      error
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
	staging := NewStagingArea()
	ing := NewIngester(parser, staging, NewIngestionGate(staging))

	err := ing.Save(context.Background(), "repo1", "main", "foo/bar.go", []byte("package foo"))
	if err != nil {
		t.Fatalf("Save returned unexpected error: %v", err)
	}
	if !parser.called {
		t.Fatal("expected ParseFile to be called")
	}

	gotNodes, ok := staging.GetStagedNodes("repo1", "main", "foo/bar.go")
	if !ok {
		t.Fatal("expected file to be staged after Save")
	}
	if len(gotNodes) != 1 {
		t.Fatalf("expected 1 staged node, got %d", len(gotNodes))
	}

	gotEdges, ok := staging.GetStagedEdges("repo1", "main", "foo/bar.go")
	if !ok {
		t.Fatal("expected staged edges to be present")
	}
	if len(gotEdges) != 1 {
		t.Fatalf("expected 1 staged edge, got %d", len(gotEdges))
	}
}

func TestIngester_Save_ParseErrorIsNonFatal(t *testing.T) {
	parser := &stubParser{err: errors.New("syntax error")}
	staging := NewStagingArea()
	ing := NewIngester(parser, staging, NewIngestionGate(staging))

	err := ing.Save(context.Background(), "repo1", "main", "bad.go", []byte("not go"))
	if err != nil {
		t.Fatalf("Save should return nil on parse error, got: %v", err)
	}

	_, staged := staging.GetStagedNodes("repo1", "main", "bad.go")
	if staged {
		t.Fatal("file should not be staged when parse fails")
	}
}

func TestIngester_DeleteFile_RemovesFromStaging(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: []*domain.Node{{}}, Edges: nil},
	}
	staging := NewStagingArea()
	ing := NewIngester(parser, staging, NewIngestionGate(staging))

	_ = ing.Save(context.Background(), "repo1", "main", "del.go", []byte("package x"))

	_, staged := staging.GetStagedNodes("repo1", "main", "del.go")
	if !staged {
		t.Fatal("expected file to be staged before delete")
	}

	ing.DeleteFile("repo1", "main", "del.go")

	_, staged = staging.GetStagedNodes("repo1", "main", "del.go")
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
	staging := NewStagingArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, staging, NewIngestionGate(staging))
	ing.SetFindingStorage(store)

	err := ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken"))
	if err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

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
	staging := NewStagingArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, staging, NewIngestionGate(staging))
	ing.SetFindingStorage(store)

	// Ingest the same broken file twice.
	for i := range 2 {
		if err := ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken")); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	got := store.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 Save calls, got %d", len(got))
	}
	// Idempotency means the same FindingID on every call — repo SQL layer
	// then collapses to one row via ON CONFLICT(finding_id, branch).
	if got[0].FindingID != got[1].FindingID {
		t.Errorf("FindingID not deterministic: %q vs %q", got[0].FindingID, got[1].FindingID)
	}
}

func TestIngester_Save_CleanParseEmitsNoFinding(t *testing.T) {
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: []*domain.Node{{}}},
	}
	staging := NewStagingArea()
	store := &recordingFindingStorage{}
	ing := NewIngester(parser, staging, NewIngestionGate(staging))
	ing.SetFindingStorage(store)

	if err := ing.Save(context.Background(), "repo1", "main", "ok.go", []byte("package x")); err != nil {
		t.Fatalf("Save: %v", err)
	}
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
	staging := NewStagingArea()
	ing := NewIngester(parser, staging, NewIngestionGate(staging))
	// Deliberately do NOT call SetFindingStorage.

	if err := ing.Save(context.Background(), "repo1", "main", "src/bad.ts", []byte("broken")); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestIngester_Save_EmptyParseResultStagesFile(t *testing.T) {
	// Even an empty parse result should call StageFile (to clear old staged state).
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: nil, Edges: nil},
	}
	staging := NewStagingArea()
	// Pre-seed staging with an old entry.
	staging.StageFile("repo1", "main", "empty.go", []*domain.Node{{}}, nil)

	ing := NewIngester(parser, staging, NewIngestionGate(staging))
	err := ing.Save(context.Background(), "repo1", "main", "empty.go", []byte("package x"))
	if err != nil {
		t.Fatalf("Save returned unexpected error: %v", err)
	}

	gotNodes, ok := staging.GetStagedNodes("repo1", "main", "empty.go")
	if !ok {
		t.Fatal("expected staging entry to exist after Save with empty result")
	}
	if len(gotNodes) != 0 {
		t.Fatalf("expected 0 staged nodes (empty result), got %d", len(gotNodes))
	}
}
