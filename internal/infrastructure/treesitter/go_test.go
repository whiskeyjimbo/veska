package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

const repoID = "test-repo"
const filePath = "pkg/foo/foo.go"

func TestParseFile_FunctionDeclaration(t *testing.T) {
	src := []byte(`package foo

func Add(a, b int) int {
	return a + b
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fn := findNodeByName(result.Nodes, "Add")
	if fn == nil {
		t.Fatal("expected a node named 'Add', got none")
		return
	}
	if fn.Kind != domain.KindFunction {
		t.Errorf("expected KindFunction, got %q", fn.Kind)
	}
	if fn.Lines == nil {
		t.Fatal("expected Lines to be set")
		return
	}
	if fn.Lines.Start != 3 {
		t.Errorf("expected Start=3, got %d", fn.Lines.Start)
	}
}

func TestParseFile_MethodDeclaration(t *testing.T) {
	src := []byte(`package foo

type Counter struct{ n int }

func (c Counter) Inc() {
	c.n++
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	method := findNodeByName(result.Nodes, "Counter.Inc")
	if method == nil {
		t.Fatal("expected a node named 'Counter.Inc', got none")
		return
	}
	if method.Kind != domain.KindMethod {
		t.Errorf("expected KindMethod, got %q", method.Kind)
	}
}

func TestParseFile_StructType(t *testing.T) {
	src := []byte(`package foo

type Point struct {
	X, Y float64
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	node := findNodeByName(result.Nodes, "Point")
	if node == nil {
		t.Fatal("expected a node named 'Point', got none")
		return
	}
	if node.Kind != domain.KindStruct {
		t.Errorf("expected KindStruct, got %q", node.Kind)
	}
}

func TestParseFile_InterfaceType(t *testing.T) {
	src := []byte(`package foo

type Writer interface {
	Write(p []byte) (n int, err error)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	node := findNodeByName(result.Nodes, "Writer")
	if node == nil {
		t.Fatal("expected a node named 'Writer', got none")
		return
	}
	if node.Kind != domain.KindInterface {
		t.Errorf("expected KindInterface, got %q", node.Kind)
	}
}

func TestParseFile_CallsEdge(t *testing.T) {
	src := []byte(`package foo

func greet() string {
	return hello()
}

func hello() string {
	return "hello"
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	greetNode := findNodeByName(result.Nodes, "greet")
	helloNode := findNodeByName(result.Nodes, "hello")
	if greetNode == nil || helloNode == nil {
		t.Fatalf("expected greet and hello nodes, got nodes: %v", nodeNames(result.Nodes))
		return
	}

	edge := findEdge(result.Edges, greetNode.ID, helloNode.ID, domain.EdgeCalls)
	if edge == nil {
		t.Errorf("expected CALLS edge from greet -> hello, none found")
	}
}

// TestParseFile_ImportsAndQualifiedCalls pins solov2-xc51.1: the parser must
// surface the file's import map and capture package-qualified calls
// (cmd.Execute()) as UnresolvedCalls carrying a PkgQualifier — the foundation
// for cross-package CALLS resolution at promotion.
func TestParseFile_ImportsAndQualifiedCalls(t *testing.T) {
	src := []byte(`package main

import (
	"fmt"
	"github.com/acme/mycli/cmd"
	flag "github.com/spf13/pflag"
	_ "embed"
)

func main() {
	cmd.Execute()
	flag.Parse()
	fmt.Println("hi")
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Import map: aliased + unaliased; blank import excluded.
	if got := result.Imports["cmd"]; got != "github.com/acme/mycli/cmd" {
		t.Errorf("imports[cmd] = %q, want github.com/acme/mycli/cmd", got)
	}
	if got := result.Imports["flag"]; got != "github.com/spf13/pflag" {
		t.Errorf("imports[flag] = %q (alias), want github.com/spf13/pflag", got)
	}
	if got := result.Imports["fmt"]; got != "fmt" {
		t.Errorf("imports[fmt] = %q, want fmt", got)
	}
	if _, ok := result.Imports["embed"]; ok {
		t.Errorf("blank import should not appear in import map: %v", result.Imports)
	}

	// Qualified calls captured with PkgQualifier.
	wantQ := map[string]string{"Execute": "cmd", "Parse": "flag", "Println": "fmt"}
	gotQ := map[string]string{}
	for _, uc := range result.UnresolvedCalls {
		if uc.PkgQualifier != "" {
			gotQ[uc.CalleeName] = uc.PkgQualifier
		}
	}
	for callee, pkg := range wantQ {
		if gotQ[callee] != pkg {
			t.Errorf("qualified call %s: qualifier = %q, want %q (all: %v)", callee, gotQ[callee], pkg, gotQ)
		}
	}
}

// TestParseFile_ReceiverSelectorCallsEdge pins solov2-q9p: when a method
// on *Server has body 's.foo()', the parser emits a CALLS edge from
// Server.Bar -> Server.foo. Without this, idiomatic Go (s.x() / s.y())
// produces zero call edges and the call graph is useless.
func TestParseFile_ReceiverSelectorCallsEdge(t *testing.T) {
	src := []byte(`package foo

type Server struct{}

func (s *Server) Foo() {
	s.bar()
}

func (s *Server) bar() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fooNode := findNodeByName(result.Nodes, "Server.Foo")
	barNode := findNodeByName(result.Nodes, "Server.bar")
	if fooNode == nil || barNode == nil {
		t.Fatalf("expected Server.Foo + Server.bar nodes, got: %v", nodeNames(result.Nodes))
	}
	if findEdge(result.Edges, fooNode.ID, barNode.ID, domain.EdgeCalls) == nil {
		t.Errorf("expected CALLS edge Server.Foo -> Server.bar (selector call on receiver); edges=%+v", result.Edges)
	}
}

func TestParseFile_NonGoFile_ReturnsEmpty(t *testing.T) {
	src := []byte(`const x = 1;`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, "pkg/foo/foo.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Errorf("expected empty result for non-Go file, got %d nodes, %d edges",
			len(result.Nodes), len(result.Edges))
	}
}

func TestParseFile_EmptyFile_ReturnsEmpty(t *testing.T) {
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, []byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Errorf("expected empty result for empty file, got %d nodes, %d edges",
			len(result.Nodes), len(result.Edges))
	}
}

func TestParseFile_MalformedGo_ReturnsEmptyNoError(t *testing.T) {
	src := []byte(`package foo

func brokenFunc( {
	// missing closing paren / brace
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("expected nil error for parse-error file, got: %v", err)
	}
	if len(result.Failures) == 0 {
		t.Fatalf("expected at least one ParseFailure for malformed Go source")
	}
}

func TestParseFile_CleanGo_NoFailures(t *testing.T) {
	src := []byte(`package foo

func Ok() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected zero failures on clean Go parse, got %d", len(result.Failures))
	}
}

func TestParseFile_ContainsEdges(t *testing.T) {
	src := []byte(`package foo

func Bar() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pkgNode := findNodeByKind(result.Nodes, domain.KindPackage)
	barNode := findNodeByName(result.Nodes, "Bar")
	if pkgNode == nil {
		t.Fatal("expected a package node")
		return
	}
	if barNode == nil {
		t.Fatal("expected a Bar node")
		return
	}

	edge := findEdge(result.Edges, pkgNode.ID, barNode.ID, domain.EdgeContains)
	if edge == nil {
		t.Errorf("expected CONTAINS edge from package -> Bar, none found")
	}
}

// --- helpers ---

func findNodeByName(nodes []*domain.Node, name string) *domain.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func findNodeByKind(nodes []*domain.Node, kind domain.NodeKind) *domain.Node {
	for _, n := range nodes {
		if n.Kind == kind {
			return n
		}
	}
	return nil
}

func findEdge(edges []*domain.Edge, src, tgt domain.NodeID, kind domain.EdgeKind) *domain.Edge {
	for _, e := range edges {
		if e.Src == src && e.Tgt == tgt && e.Kind == kind {
			return e
		}
	}
	return nil
}

func nodeNames(nodes []*domain.Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}
