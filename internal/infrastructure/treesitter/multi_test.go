package treesitter

import (
	"context"
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestMultiParser_SupportedExtensionsUnion pins that a MultiParser reports the
// sorted union of its sub-parsers' extensions — the set the cold scan sources
// its walk filter from (solov2-xde2.7).
func TestMultiParser_SupportedExtensionsUnion(t *testing.T) {
	m := NewMultiParser(NewGoParser(), NewTSParser())
	got := m.SupportedExtensions()
	want := []string{".go", ".ts", ".tsx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedExtensions = %v, want %v", got, want)
	}
}

// TestMultiParser_RoutesByExtension confirms each extension reaches the
// sub-parser that claims it: a .go file yields a Go module node, a .ts file a
// TypeScript module node.
func TestMultiParser_RoutesByExtension(t *testing.T) {
	m := NewMultiParser(NewGoParser(), NewTSParser())

	goRes, err := m.ParseFile(context.Background(), "r", "a.go", []byte("package a\n\nfunc F() {}\n"))
	if err != nil {
		t.Fatalf("ParseFile(.go): %v", err)
	}
	if !hasLanguage(goRes.Nodes, "go") {
		t.Fatalf(".go file produced no Go nodes: %+v", goRes.Nodes)
	}

	tsRes, err := m.ParseFile(context.Background(), "r", "b.ts", []byte("export function f() {}\n"))
	if err != nil {
		t.Fatalf("ParseFile(.ts): %v", err)
	}
	if !hasLanguage(tsRes.Nodes, "typescript") {
		t.Fatalf(".ts file produced no TypeScript nodes: %+v", tsRes.Nodes)
	}
}

// TestMultiParser_UnknownExtensionEmpty confirms a file no sub-parser claims
// returns an empty ParseResult and nil error.
func TestMultiParser_UnknownExtensionEmpty(t *testing.T) {
	m := NewMultiParser(NewGoParser(), NewTSParser())
	res, err := m.ParseFile(context.Background(), "r", "README.md", []byte("# hi"))
	if err != nil {
		t.Fatalf("ParseFile(.md): %v", err)
	}
	if len(res.Nodes) != 0 || len(res.Edges) != 0 {
		t.Fatalf("unknown extension produced nodes/edges: %+v", res)
	}
}

func hasLanguage(nodes []*domain.Node, lang string) bool {
	for _, n := range nodes {
		if n.Language != nil && *n.Language == lang {
			return true
		}
	}
	return false
}
