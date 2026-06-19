// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"context"
	"reflect"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestMultiParser_SupportedExtensionsUnion verifies that MultiParser returns the sorted union
// of all extensions supported by its sub-parsers.
func TestMultiParser_SupportedExtensionsUnion(t *testing.T) {
	m := NewMultiParser(NewGoParser(), NewTSParser())
	got := m.SupportedExtensions()
	want := []string{".go", ".ts", ".tsx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedExtensions = %v, want %v", got, want)
	}
}

// TestMultiParser_RoutesByExtension verifies that parser routing correctly delegates
// file parsing to the sub-parser registered for the file extension.
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

// TestMultiParser_UnknownExtensionEmpty verifies that parsing a file with an
// unsupported extension returns an empty ParseResult.
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
