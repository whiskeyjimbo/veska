package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TSParser is a tree-sitter-backed implementation of ports.CodeParser for TypeScript
// and TSX source files. Each ParseFile call is stateless: a fresh sitter.Parser is
// created per call.
type TSParser struct{}

// NewTSParser returns a new TSParser.
func NewTSParser() *TSParser {
	return &TSParser{}
}

// ParseFile parses TypeScript (.ts) or TSX (.tsx) source and returns the Nodes and
// Edges extracted from it. Other file extensions return an empty ParseResult and nil
// error.
func (p *TSParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".ts" && ext != ".tsx" {
		return &domain.ParseResult{}, nil
	}
	if len(src) == 0 {
		return &domain.ParseResult{}, nil
	}

	lang := typescript.GetLanguage()
	if ext == ".tsx" {
		lang = tsx.GetLanguage()
	}

	parser := sitter.NewParser()
	parser.SetLanguage(lang)

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	result := &domain.ParseResult{}

	// Surface syntax errors as ParseFailures. Unlike the Go parser we still
	// extract whatever symbols tree-sitter could recover — TS/TSX trees with
	// localized errors typically still expose valid top-level declarations.
	if hasErrorNode(root) {
		result.Failures = append(result.Failures, firstErrorFailure(root))
	}

	// --- module node (one per file) ---
	base := filepath.Base(path)
	modName := strings.TrimSuffix(base, filepath.Ext(base))
	modID := nodeID(repoID, path, domain.KindModule, modName)
	modNode, err := domain.NewNode(modID, path, modName, domain.KindModule,
		domain.WithLanguage("typescript"),
	)
	if err == nil {
		result.Nodes = append(result.Nodes, modNode)
	}

	// --- symbol nodes ---
	symbolByName := map[string]*domain.Node{}
	// track class names for method association: className -> node
	classNames := map[string]bool{}

	extractTSSymbols(root, src, repoID, path, result, symbolByName, classNames)

	// --- CONTAINS edges: module -> each symbol ---
	for _, n := range result.Nodes {
		if n == modNode {
			continue
		}
		e, err := domain.NewEdge(modNode.ID, n.ID, domain.EdgeContains,
			domain.WithConfidence(domain.Definite),
		)
		if err == nil {
			result.Edges = append(result.Edges, e)
		}
	}

	// --- CALLS edges ---
	callEdges := extractTSCallEdges(root, src, symbolByName)
	result.Edges = append(result.Edges, callEdges...)

	// --- chunk index over non-declaration regions (solov2-jyt) ---
	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	// --- TODO/FIXME markers (language-agnostic lexical scan) ---
	result.Todos = scanTodos(src)

	return result, nil
}

// extractTSSymbols walks the AST top-level statements and extracts functions,
// classes, interfaces, and methods.
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
		processTopLevelNode(child, src, repoID, path, result, symbolByName, classNames)
	}
}

// processTopLevelNode handles a single top-level AST node, recursing through
// export_statement wrappers as needed.
func processTopLevelNode(
	node *sitter.Node,
	src []byte,
	repoID, path string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
	classNames map[string]bool,
) {
	switch node.Type() {
	case "function_declaration":
		n := parseTSFunctionDecl(node, src, repoID, path)
		if n != nil {
			result.Nodes = append(result.Nodes, n)
			symbolByName[n.Name] = n
		}

	case "class_declaration":
		className, classNode := parseTSClassDecl(node, src, repoID, path)
		if classNode != nil {
			result.Nodes = append(result.Nodes, classNode)
			symbolByName[classNode.Name] = classNode
			classNames[className] = true
		}
		// extract methods from the class body
		bodyNode := node.ChildByFieldName("body")
		if bodyNode != nil && className != "" {
			parseTSClassBody(bodyNode, src, repoID, path, className, result, symbolByName)
		}

	case "interface_declaration":
		n := parseTSInterfaceDecl(node, src, repoID, path)
		if n != nil {
			result.Nodes = append(result.Nodes, n)
			symbolByName[n.Name] = n
		}

	case "export_statement":
		// unwrap: export default / export function / export class
		inner := node.ChildByFieldName("declaration")
		if inner == nil {
			// try second child (export default <expr>)
			if node.ChildCount() >= 2 {
				inner = node.Child(1)
			}
		}
		if inner != nil {
			processTopLevelNode(inner, src, repoID, path, result, symbolByName, classNames)
		}

	case "lexical_declaration", "variable_declaration":
		// const Foo = () => { ... }  arrow function assigned to variable
		parseTSArrowFunctions(node, src, repoID, path, result, symbolByName)
	}
}

// parseTSFunctionDecl extracts a function_declaration node.
func parseTSFunctionDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(id, path, name, domain.KindFunction,
		domain.WithLanguage("typescript"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
	)
	if err != nil {
		return nil
	}
	return n
}

// parseTSClassDecl extracts a class_declaration node. Returns className and node.
func parseTSClassDecl(node *sitter.Node, src []byte, repoID, path string) (string, *domain.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return "", nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindClass, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(id, path, name, domain.KindClass,
		domain.WithLanguage("typescript"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
	)
	if err != nil {
		return name, nil
	}
	return name, n
}

// parseTSClassBody walks a class_body and extracts method_definition nodes.
func parseTSClassBody(
	body *sitter.Node,
	src []byte,
	repoID, path, className string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
) {
	count := int(body.ChildCount())
	for i := range count {
		child := body.Child(i)
		if child.Type() == "method_definition" {
			n := parseTSMethodDef(child, src, repoID, path, className)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		}
	}
}

// parseTSMethodDef extracts a method_definition inside a class body.
func parseTSMethodDef(node *sitter.Node, src []byte, repoID, path, className string) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
	fullName := className + "." + methodName
	id := nodeID(repoID, path, domain.KindMethod, fullName)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(id, path, fullName, domain.KindMethod,
		domain.WithLanguage("typescript"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
	)
	if err != nil {
		return nil
	}
	return n
}

// parseTSInterfaceDecl extracts an interface_declaration node.
func parseTSInterfaceDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])
	id := nodeID(repoID, path, domain.KindInterface, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])
	n, err := domain.NewNode(id, path, name, domain.KindInterface,
		domain.WithLanguage("typescript"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
	)
	if err != nil {
		return nil
	}
	return n
}

// parseTSArrowFunctions extracts arrow functions assigned to variables:
//
//	const greet = (name: string) => { ... }
func parseTSArrowFunctions(
	node *sitter.Node,
	src []byte,
	repoID, path string,
	result *domain.ParseResult,
	symbolByName map[string]*domain.Node,
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
		n, err := domain.NewNode(id, path, name, domain.KindFunction,
			domain.WithLanguage("typescript"),
			domain.WithLines(lr),
			domain.WithRawContent(raw),
		)
		if err != nil {
			continue
		}
		result.Nodes = append(result.Nodes, n)
		symbolByName[name] = n
	}
}

// collectTSCallsFromClassBody walks each method_definition under a
// class body and emits CALLS edges. Calls of the form this.foo() are
// rewritten to className.foo and resolved against the file's symbol
// map — bare-identifier calls (e.g. helper()) resolve directly. This
// is the TS analogue of the Go receiver-selector rewrite (solov2-q9p)
// and is what makes intra-class dependencies show up in eng_get_call_chain
// for TS code (solov2-gv6).
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
		for _, ref := range collectCallNames(bodyNode, src, "this", className) {
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
			e, err := domain.NewEdge(caller.ID, calleeNode.ID, domain.EdgeCalls,
				domain.WithConfidence(domain.Probable),
			)
			if err == nil {
				edges = append(edges, e)
			}
		}
	}
	return edges
}

// extractTSCallEdges walks the entire AST finding call_expression nodes inside
// function/method bodies and emits EdgeCalls when the callee is known in the file.
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
		// Each method inside the class is its own caller. Resolve
		// this.foo() against the class's own method namespace via
		// collectCallNames(recvName="this", recvType=className) —
		// mirrors Go's receiver-selector resolution (solov2-gv6).
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
		// recurse into the declaration
		inner := node.ChildByFieldName("declaration")
		if inner == nil && node.ChildCount() >= 2 {
			inner = node.Child(1)
		}
		if inner != nil {
			edges = append(edges, collectTSCallsFromTopLevel(inner, src, symbols)...)
		}
		return edges
	case "lexical_declaration", "variable_declaration":
		// arrow functions
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
			callNames := collectCallNames(bodyNode, src, "", "")
			for _, ref := range callNames {
				if ref.pkg != "" {
					continue
				}
				calleeNode, ok := symbols[ref.name]
				if !ok {
					continue
				}
				e, err := domain.NewEdge(caller.ID, calleeNode.ID, domain.EdgeCalls,
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

	callNames := collectCallNames(bodyNode, src, "", "")
	for _, ref := range callNames {
		if ref.pkg != "" {
			continue
		}
		calleeNode, ok := symbols[ref.name]
		if !ok {
			continue
		}
		e, err := domain.NewEdge(callerNode.ID, calleeNode.ID, domain.EdgeCalls,
			domain.WithConfidence(domain.Probable),
		)
		if err == nil {
			edges = append(edges, e)
		}
	}
	return edges
}
