// go_query.go — query-driven Go parser (solov2-1yev phase 1).
//
// This is the new tree-sitter Query API path. It coexists with go.go
// (the legacy hand-rolled walkers) so an equivalence harness can diff
// the two implementations on the same fixtures before we flip the
// default. Each phase of the rewrite plugs another extractor into this
// file; phase 1 ships only top-level function declarations.
//
// Construction:
//
//	parser := NewGoQueryParser()   // satisfies ports.CodeParser
//	// ... same usage as NewGoParser()
//
// Until equivalence is validated across the entire fixture corpus, the
// daemon's composition root keeps using NewGoParser. Phase 5 flips the
// default and drops the legacy path.
package treesitter

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GoQueryParser is the query-driven implementation of ports.CodeParser
// for Go. Construction is cheap (no parser pool yet — phase 1 reuses
// the package-level parserPool from parser_pool.go to keep cgo init
// cost amortised). It is safe for concurrent use.
type GoQueryParser struct{}

// NewGoQueryParser constructs a query-driven Go parser. Until phase 5
// the daemon wires NewGoParser; this constructor exists so the
// equivalence harness and benchmark suite can exercise the new path.
func NewGoQueryParser() *GoQueryParser {
	return &GoQueryParser{}
}

// ParseFile mirrors GoParser.ParseFile's contract: parse src as Go
// source and return a ParseResult of nodes + edges + diagnostics.
// Phase 1 implementation: package node + top-level function
// declarations via queries/go/symbols.scm. Everything else (methods,
// types, calls, imports, ...) is produced as empty/nil so the diff
// harness can compare extractor-by-extractor as phases land.
func (p *GoQueryParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (domain.ParseResult, error) {
	tsParser := goParserPool.Get().(*sitter.Parser)
	defer goParserPool.Put(tsParser)

	tree, err := tsParser.ParseCtx(ctx, nil, src)
	if err != nil {
		return domain.ParseResult{}, fmt.Errorf("go_query: parse %s: %w", path, err)
	}
	defer tree.Close()
	root := tree.RootNode()

	// solov2-0kv6 mirror: when go/parser accepts the file, tree-sitter's
	// ERROR nodes are false positives. Track parseAccepted so the
	// per-decl skip can re-include declarations the recursive walker
	// would have accepted.
	parseAccepted := true
	if _, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution); perr != nil {
		parseAccepted = false
	}

	result := domain.ParseResult{}

	// Package node — the legacy parser emits this; phase 1 keeps parity
	// so the equivalence harness doesn't trip on its absence.
	if pkgNode := buildPackageNode(root, src, repoID, path); pkgNode != nil {
		result.Nodes = append(result.Nodes, pkgNode)
	}

	// Function declarations via the symbols.scm query.
	q, qerr := compileEmbeddedQuery(tsgo.GetLanguage(), "go", "symbols")
	if qerr != nil {
		return domain.ParseResult{}, qerr
	}
	for _, m := range runQuery(q, root) {
		declNode := m.node("function.decl")
		nameNode := m.node("function.name")
		if declNode == nil || nameNode == nil {
			continue
		}
		// solov2-7nkm / solov2-0kv6: a function inside an ERROR subtree
		// has unreliable name/body bytes; the legacy parser skips it
		// UNLESS go/parser accepted the whole file (then ts ERROR nodes
		// are false positives).
		if !parseAccepted && hasErrorNode(declNode) {
			continue
		}
		n := buildFunctionNodeFromCaptures(declNode, nameNode, src, repoID, path)
		if n != nil {
			result.Nodes = append(result.Nodes, n)
		}
	}

	return result, nil
}

// buildFunctionNodeFromCaptures mirrors parseFunctionDecl in go.go but
// takes the already-located decl + name nodes from a query match. The
// rest (lines, raw content, exported flag, signature) is byte-for-byte
// identical to the legacy path — that is the explicit equivalence
// contract for phase 1.
func buildFunctionNodeFromCaptures(declNode, nameNode *sitter.Node, src []byte, repoID, path string) *domain.Node {
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(declNode)
	raw := string(src[declNode.StartByte():declNode.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(name)),
	}
	if sig := extractSignature(declNode, src); sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}
	n, err := domain.NewNode(id, path, name, domain.KindFunction, opts...)
	if err != nil {
		return nil
	}
	return n
}

// buildPackageNode produces the package-clause node the legacy parser
// emits at the top of every Go file. Extracted into a helper here so
// the query parser keeps parity until packages get their own .scm
// (phase 2 candidate).
func buildPackageNode(root *sitter.Node, src []byte, repoID, path string) *domain.Node {
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child.Type() != "package_clause" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			// Some Go grammar versions expose the identifier as a plain
			// named child rather than via the "name" field. Walk for it.
			named := int(child.NamedChildCount())
			for j := range named {
				c := child.NamedChild(j)
				if c != nil && c.Type() == "package_identifier" {
					nameNode = c
					break
				}
			}
		}
		if nameNode == nil {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		id := nodeID(repoID, path, domain.KindPackage, name)
		// Legacy parser intentionally omits Lines on the package node —
		// extractPackageName + NewNode-without-WithLines (go.go ~L92).
		// Match that exactly so the equivalence harness stays green.
		n, err := domain.NewNode(id, path, name, domain.KindPackage,
			domain.WithLanguage("go"),
		)
		if err != nil {
			return nil
		}
		return n
	}
	return nil
}
