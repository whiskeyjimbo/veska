// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TSParser is a tree-sitter-backed implementation of CodeParser for TypeScript and TSX
// source files. It reuses parser instances from a sync.Pool to amortize setup costs.
type TSParser struct{}

// NewTSParser returns a new TSParser.
func NewTSParser() *TSParser {
	return &TSParser{}
}

// SupportedExtensions returns the file extensions supported by TSParser.
func (p *TSParser) SupportedExtensions() []string { return []string{".ts", ".tsx"} }

// ParseFile parses TypeScript or TSX source code and returns the extracted nodes and edges.
func (p *TSParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".ts" && ext != ".tsx" {
		return &domain.ParseResult{}, nil
	}
	if len(src) == 0 {
		return &domain.ParseResult{}, nil
	}

	pool := tsParserPool
	if ext == ".tsx" {
		pool = tsxParserPool
	}
	parser := pool.Get().(*sitter.Parser)
	defer pool.Put(parser)

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	result := &domain.ParseResult{}

	// We surface syntax errors as ParseFailures but still attempt to extract symbols
	// that tree-sitter was able to recover.
	if hasErrorNode(root) {
		result.Failures = append(result.Failures, firstErrorFailure(root))
	}

	base := filepath.Base(path)
	modName := strings.TrimSuffix(base, filepath.Ext(base))
	modID := nodeID(repoID, path, domain.KindModule, modName)
	modNode, err := domain.NewNode(domain.NodeSpec{ID: modID, Path: path, Name: modName, Kind: domain.KindModule}, domain.WithLanguage("typescript"))
	if err == nil {
		result.Nodes = append(result.Nodes, modNode)
	}

	symbolByName := map[string]*domain.Node{}
	// track class names for method association: className -> node
	classNames := map[string]bool{}

	extractTSSymbols(root, src, repoID, path, result, symbolByName, classNames)

	for _, n := range result.Nodes {
		if n == modNode {
			continue
		}
		e, err := domain.NewEdge(domain.EdgeSpec{
			Src:  modNode.ID,
			Tgt:  n.ID,
			Kind: domain.EdgeContains,
		},
			domain.WithConfidence(domain.Definite),
		)
		if err == nil {
			result.Edges = append(result.Edges, e)
		}
	}

	callEdges := extractTSCallEdges(root, src, symbolByName)
	result.Edges = append(result.Edges, callEdges...)

	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	result.Todos = scanTodos(src)

	return result, nil
}

// extractTSSymbols walks the AST to extract function, class, interface, and method symbols.
func extractTSSymbols(
	root *sitter.Node,
	src []byte,
	repoID, path string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
	classNames map[string]bool,
) {
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		// Top-level declarations are treated as unexported unless wrapped in an export statement.
		processTopLevelNode(child, src, repoID, path, result, symbolByName, classNames, false)
	}
}

// processTopLevelNode handles a single top-level AST node, recursing into export statements.
func processTopLevelNode(
	node *sitter.Node,
	src []byte,
	repoID, path string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
	classNames map[string]bool,
	exported bool,
) {
	// We skip declarations whose subtrees contain syntax errors to avoid extracting
	// unreliable names or signatures, while still indexing clean sibling declarations.
	if hasErrorNode(node) {
		return
	}
	switch node.Type() {
	case "function_declaration":
		n := parseTSFunctionDecl(node, src, repoID, path, exported)
		if n != nil {
			result.Nodes = append(result.Nodes, n)
			symbolByName[n.Name] = n
		}

	case "class_declaration":
		className, classNode := parseTSClassDecl(node, src, repoID, path, exported)
		if classNode != nil {
			result.Nodes = append(result.Nodes, classNode)
			symbolByName[classNode.Name] = classNode
			classNames[className] = true
		}
		// We extract methods from the class body, propagating the class's export status.
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil && className != "" {
			parseTSClassBody(bodyNode, src, repoID, path, className, result, symbolByName, exported)
		}

	case "interface_declaration":
		n := parseTSInterfaceDecl(node, src, repoID, path, exported)
		if n != nil {
			result.Nodes = append(result.Nodes, n)
			symbolByName[n.Name] = n
		}

	case "export_statement":

		inner := node.ChildByFieldName("declaration")
		if inner == nil {

			if node.ChildCount() >= 2 {
				inner = node.Child(1)
			}
		}
		if inner != nil {
			processTopLevelNode(inner, src, repoID, path, result, symbolByName, classNames, true)
		}

	case "lexical_declaration", "variable_declaration":

		parseTSArrowFunctions(node, src, repoID, path, result, symbolByName, exported)
	}
}

func parseTSFunctionDecl(node *sitter.Node, src []byte, repoID, path string, exported bool) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindFunction}, domain.WithLanguage("typescript"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(exported))
	if err != nil {
		return nil
	}
	return n
}

// parseTSClassDecl extracts a class declaration node, returning the class name and the node.
func parseTSClassDecl(node *sitter.Node, src []byte, repoID, path string, exported bool) (string, *domain.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return "", nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindClass, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindClass}, domain.WithLanguage("typescript"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(exported))
	if err != nil {
		return name, nil
	}
	return name, n
}

// parseTSClassBody extracts method definitions from a class body, propagating the class's
// export status.
func parseTSClassBody(
	body *sitter.Node,
	src []byte,
	repoID, path, className string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
	classExported bool,
) {
	count := int(body.ChildCount())
	for i := range count {
		child := body.Child(i)
		if child.Type() == "method_definition" {
			n := parseTSMethodDef(child, src, repoID, path, className, classExported)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		}
	}
}

func parseTSMethodDef(node *sitter.Node, src []byte, repoID, path, className string, exported bool) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
	fullName := className + "." + methodName
	id := nodeID(repoID, path, domain.KindMethod, fullName)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: fullName, Kind: domain.KindMethod}, domain.WithLanguage("typescript"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(exported))
	if err != nil {
		return nil
	}
	return n
}

func parseTSInterfaceDecl(node *sitter.Node, src []byte, repoID, path string, exported bool) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindInterface, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindInterface}, domain.WithLanguage("typescript"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(exported))
	if err != nil {
		return nil
	}
	return n
}

// parseTSArrowFunctions extracts arrow functions assigned to variables (for example,
// `const greet = () => {}`).
func parseTSArrowFunctions(
	node *sitter.Node,
	src []byte,
	repoID, path string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
	exported bool,
) {
	count := int(node.ChildCount())
	for i := range count {
		child := node.Child(i)
		if child.Type() != "variable_declarator" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		valueNode := child.ChildByFieldName("value")
		if nameNode == nil || valueNode == nil {
			continue
		}
		if valueNode.Type() != "arrow_function" {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		id := nodeID(repoID, path, domain.KindFunction, name)
		lr := lineRange(child)
		raw := string(src[child.StartByte():child.EndByte()])
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindFunction}, domain.WithLanguage("typescript"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(exported))
		if err != nil {
			continue
		}
		result.Nodes = append(result.Nodes, n)
		symbolByName[name] = n
	}
}

// collectTSCallsFromClassBody extracts calls from methods inside a class body. It rewrites
// `this.Method` calls to `Class.Method` to resolve them against local symbols.
func collectTSCallsFromClassBody(body *sitter.Node, src []byte, symbols map[string]*domain.Node, className string) []*domain.Edge {
	var edges []*domain.Edge
	seen := make(map[string]bool)
	count := int(body.ChildCount())
	for i := range count {
		child := body.Child(i)
		if child.Type() != "method_definition" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
		caller := symbols[className+"."+methodName]
		if caller == nil {
			continue
		}
		bodyNode := child.ChildByFieldName("body")
		if bodyNode == nil {
			continue
		}
		for _, ref := range collectCallNames(bodyNode, src, "this", className, nil) {
			// Package-qualified calls are skipped since TypeScript parsing does not perform
			// cross-package symbol resolution.
			if ref.pkg != "" {
				continue
			}
			calleeNode, ok := symbols[ref.name]
			if !ok {
				continue
			}
			key := string(caller.ID) + "->" + string(calleeNode.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			e, err := domain.NewEdge(domain.EdgeSpec{
				Src:  caller.ID,
				Tgt:  calleeNode.ID,
				Kind: domain.EdgeCalls,
			},
				domain.WithConfidence(domain.Probable),
			)
			if err == nil {
				edges = append(edges, e)
			}
		}
	}
	return edges
}

// extractTSCallEdges extracts call edges from function and method bodies.
func extractTSCallEdges(root *sitter.Node, src []byte, symbols map[string]*domain.Node) []*domain.Edge {
	var edges []*domain.Edge

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		edges = append(edges, collectTSCallsFromTopLevel(child, src, symbols)...)
	}
	return edges
}

func collectTSCallsFromTopLevel(node *sitter.Node, src []byte, symbols map[string]*domain.Node) []*domain.Edge {
	var edges []*domain.Edge

	var callerNode *domain.Node

	switch node.Type() {
	case "function_declaration":
		nameNode := node.ChildByFieldName("name")
		if nameNode != nil {
			callerNode = symbols[string(src[nameNode.StartByte():nameNode.EndByte()])]
		}
	case "class_declaration":
		// Methods inside class declarations are treated as caller symbols. We resolve `this`
		// references to the enclosing class namespace.
		nameNode := node.ChildByFieldName("name")
		if nameNode == nil {
			return edges
		}
		className := string(src[nameNode.StartByte():nameNode.EndByte()])
		bodyNode := node.ChildByFieldName("body")
		if bodyNode == nil {
			return edges
		}
		edges = append(edges, collectTSCallsFromClassBody(bodyNode, src, symbols, className)...)
		return edges
	case "export_statement":

		inner := node.ChildByFieldName("declaration")
		if inner == nil && node.ChildCount() >= 2 {
			inner = node.Child(1)
		}
		if inner != nil {
			edges = append(edges, collectTSCallsFromTopLevel(inner, src, symbols)...)
		}
		return edges
	case "lexical_declaration", "variable_declaration":

		cnt := int(node.ChildCount())
		for i := range cnt {
			decl := node.Child(i)
			if decl.Type() != "variable_declarator" {
				continue
			}
			nameNode := decl.ChildByFieldName("name")
			valueNode := decl.ChildByFieldName("value")
			if nameNode == nil || valueNode == nil || valueNode.Type() != "arrow_function" {
				continue
			}
			caller := symbols[string(src[nameNode.StartByte():nameNode.EndByte()])]
			if caller == nil {
				continue
			}
			bodyNode := valueNode.ChildByFieldName("body")
			if bodyNode == nil {
				continue
			}
			callNames := collectCallNames(bodyNode, src, "", "", nil)
			for _, ref := range callNames {
				if ref.pkg != "" {
					continue
				}
				calleeNode, ok := symbols[ref.name]
				if !ok {
					continue
				}
				e, err := domain.NewEdge(domain.EdgeSpec{
					Src:  caller.ID,
					Tgt:  calleeNode.ID,
					Kind: domain.EdgeCalls,
				},
					domain.WithConfidence(domain.Probable),
				)
				if err == nil {
					edges = append(edges, e)
				}
			}
		}
		return edges
	}

	if callerNode == nil {
		return edges
	}

	bodyNode := node.ChildByFieldName("body")
	if bodyNode == nil {
		return edges
	}

	callNames := collectCallNames(bodyNode, src, "", "", nil)
	for _, ref := range callNames {
		if ref.pkg != "" {
			continue
		}
		calleeNode, ok := symbols[ref.name]
		if !ok {
			continue
		}
		e, err := domain.NewEdge(domain.EdgeSpec{
			Src:  callerNode.ID,
			Tgt:  calleeNode.ID,
			Kind: domain.EdgeCalls,
		},
			domain.WithConfidence(domain.Probable),
		)
		if err == nil {
			edges = append(edges, e)
		}
	}
	return edges
}
