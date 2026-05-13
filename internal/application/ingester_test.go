package application

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

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
	ing := NewIngester(parser, staging)

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
	ing := NewIngester(parser, staging)

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
	ing := NewIngester(parser, staging)

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

func TestIngester_Save_EmptyParseResultStagesFile(t *testing.T) {
	// Even an empty parse result should call StageFile (to clear old staged state).
	parser := &stubParser{
		result: &domain.ParseResult{Nodes: nil, Edges: nil},
	}
	staging := NewStagingArea()
	// Pre-seed staging with an old entry.
	staging.StageFile("repo1", "main", "empty.go", []*domain.Node{{}}, nil)

	ing := NewIngester(parser, staging)
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
