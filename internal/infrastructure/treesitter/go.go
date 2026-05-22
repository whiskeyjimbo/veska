package treesitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// goExported reports whether a Go identifier is exported — its first rune is an
// uppercase letter. Names like "Receiver.Method" should be passed as the bare
// method-name segment by the caller.
func goExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

// GoParser is a tree-sitter-backed implementation of ports.CodeParser for Go source files.
// Each ParseFile call is stateless: a fresh sitter.Parser is created per call.
type GoParser struct{}

// NewGoParser returns a new GoParser.
func NewGoParser() *GoParser {
	return &GoParser{}
}

// ParseFile parses the Go source in src and returns the Nodes and Edges extracted from it.
// Non-Go files (by extension) return an empty ParseResult and nil error.
// If the tree-sitter parse produces error nodes the result is empty (parse errors are non-fatal).
func (p *GoParser) ParseFile(ctx context.Context, repoID, path string, src []byte) (*domain.ParseResult, error) {
	if filepath.Ext(path) != ".go" {
		return &domain.ParseResult{}, nil
	}
	if len(src) == 0 {
		return &domain.ParseResult{}, nil
	}

	parser := sitter.NewParser()
	parser.SetLanguage(golang.GetLanguage())

	tree, err := parser.ParseCtx(ctx, nil, src)
	if err != nil {
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{{Line: 0, Message: "tree-sitter parse error: " + err.Error()}},
		}, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	// If the root itself has errors, surface a parse failure and bail out of
	// symbol extraction (partial trees produce noisy/incorrect symbols).
	if hasErrorNode(root) {
		return &domain.ParseResult{
			Failures: []domain.ParseFailure{firstErrorFailure(root)},
		}, nil
	}

	result := &domain.ParseResult{}

	// --- package node ---
	pkgName := extractPackageName(root, src)
	var pkgNode *domain.Node
	if pkgName != "" {
		id := nodeID(repoID, path, domain.KindPackage, pkgName)
		n, err := domain.NewNode(id, path, pkgName, domain.KindPackage,
			domain.WithLanguage("go"),
		)
		if err == nil {
			pkgNode = n
			result.Nodes = append(result.Nodes, pkgNode)
		}
	}

	// --- symbol nodes indexed by name for CALLS resolution ---
	symbolByName := map[string]*domain.Node{}

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		switch child.Type() {
		case "function_declaration":
			n := parseFunctionDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		case "method_declaration":
			n := parseMethodDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		case "type_declaration":
			n := parseTypeDecl(child, src, repoID, path)
			if n != nil {
				result.Nodes = append(result.Nodes, n)
				symbolByName[n.Name] = n
			}
		}
	}

	// --- CONTAINS edges: package -> each symbol ---
	if pkgNode != nil {
		for _, n := range result.Nodes {
			if n == pkgNode {
				continue
			}
			e, err := domain.NewEdge(pkgNode.ID, n.ID, domain.EdgeContains,
				domain.WithConfidence(domain.Definite),
			)
			if err == nil {
				result.Edges = append(result.Edges, e)
			}
		}
	}

	// --- CALLS edges ---
	// Resolved-locally calls become edges; calls naming a symbol that's
	// not in this file's map (likely another file in the same Go package)
	// surface as UnresolvedCalls and get bound at promotion time
	// (solov2-2at).
	callEdges, unresolved := extractCallEdges(root, src, symbolByName)
	result.Edges = append(result.Edges, callEdges...)
	result.UnresolvedCalls = unresolved

	// --- chunk index over non-declaration regions (solov2-jyt) ---
	// Emitted AFTER CALLS so the symbol set used for CALLS resolution
	// stays purely declarative — chunks aren't callable symbols.
	result.Nodes = append(result.Nodes, chunkFile(repoID, path, src, result.Nodes)...)

	// --- TODO/FIXME markers (language-agnostic lexical scan) ---
	result.Todos = scanTodos(src)

	return result, nil
}

// ----- node extraction helpers -----

func parseFunctionDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	name := string(src[nameNode.StartByte():nameNode.EndByte()])

	id := nodeID(repoID, path, domain.KindFunction, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		domain.WithExported(goExported(name)),
	}

	sig := extractSignature(node, src)
	if sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}

	n, err := domain.NewNode(id, path, name, domain.KindFunction, opts...)
	if err != nil {
		return nil
	}
	return n
}

func parseMethodDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	// receiver field contains the parameter_list with the receiver spec
	receiverNode := node.ChildByFieldName("receiver")
	nameNode := node.ChildByFieldName("name")
	if receiverNode == nil || nameNode == nil {
		return nil
	}

	receiverType := extractReceiverType(receiverNode, src)
	methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
	name := receiverType + "." + methodName

	id := nodeID(repoID, path, domain.KindMethod, name)
	lr := lineRange(node)
	raw := string(src[node.StartByte():node.EndByte()])

	opts := []domain.NodeOption{
		domain.WithLanguage("go"),
		domain.WithLines(lr),
		domain.WithRawContent(raw),
		// A method is exported when its method name (after "Receiver.") is
		// capitalised; the receiver type's casing is irrelevant.
		domain.WithExported(goExported(methodName)),
	}

	sig := extractSignature(node, src)
	if sig != "" {
		opts = append(opts, domain.WithSignature(sig))
	}

	n, err := domain.NewNode(id, path, name, domain.KindMethod, opts...)
	if err != nil {
		return nil
	}
	return n
}

func parseTypeDecl(node *sitter.Node, src []byte, repoID, path string) *domain.Node {
	// type_declaration -> type_spec -> name + type
	count := int(node.ChildCount())
	for i := range count {
		spec := node.Child(i)
		if spec.Type() != "type_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		typeNode := spec.ChildByFieldName("type")
		if nameNode == nil || typeNode == nil {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])

		kind := domain.KindType
		switch typeNode.Type() {
		case "struct_type":
			kind = domain.KindStruct
		case "interface_type":
			kind = domain.KindInterface
		}

		id := nodeID(repoID, path, kind, name)
		lr := lineRange(node)
		raw := string(src[node.StartByte():node.EndByte()])

		n, err := domain.NewNode(id, path, name, kind,
			domain.WithLanguage("go"),
			domain.WithLines(lr),
			domain.WithRawContent(raw),
			domain.WithExported(goExported(name)),
		)
		if err != nil {
			return nil
		}
		return n
	}
	return nil
}

// ----- CALLS extraction -----

// extractCallEdges walks the entire AST looking for call_expression nodes inside
// function/method bodies and emits EdgeCalls when the callee is known in the file.
func extractCallEdges(root *sitter.Node, src []byte, symbols map[string]*domain.Node) ([]*domain.Edge, []domain.UnresolvedCall) {
	var edges []*domain.Edge
	var unresolved []domain.UnresolvedCall
	seen := make(map[string]bool) // dedupe same caller→callee within a file
	seenU := make(map[string]bool)

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		var callerNode *domain.Node
		var recvName, recvType string

		switch child.Type() {
		case "function_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			callerNode = symbols[string(src[nameNode.StartByte():nameNode.EndByte()])]
		case "method_declaration":
			receiverNode := child.ChildByFieldName("receiver")
			nameNode := child.ChildByFieldName("name")
			if receiverNode == nil || nameNode == nil {
				continue
			}
			recvName, recvType = extractReceiverBinding(receiverNode, src)
			name := recvType + "." + string(src[nameNode.StartByte():nameNode.EndByte()])
			callerNode = symbols[name]
		default:
			continue
		}

		if callerNode == nil {
			continue
		}

		bodyNode := child.ChildByFieldName("body")
		if bodyNode == nil {
			continue
		}

		// Identifier calls (foo()) — resolve directly against the file's
		// symbol map. Selector calls on the receiver (s.foo() inside
		// a method on *Server) — rewrite as Server.foo and resolve too
		// (solov2-q9p). Cross-package selector calls (pkg.Bar) are not
		// resolved here; they need a cross-repo IMPORTS graph
		// (solov2-1gj) before we can attach a target.
		callNames := collectCallNames(bodyNode, src, recvName, recvType)
		for _, callee := range callNames {
			calleeNode, ok := symbols[callee]
			if !ok {
				// Stash for cross-file resolution at promotion time
				// (solov2-2at). Dedupe per (caller, callee-name) so
				// repeated call sites yield one resolution attempt.
				key := string(callerNode.ID) + "→" + callee
				if seenU[key] {
					continue
				}
				seenU[key] = true
				unresolved = append(unresolved, domain.UnresolvedCall{
					CallerID:   callerNode.ID,
					CalleeName: callee,
				})
				continue
			}
			key := string(callerNode.ID) + "->" + string(calleeNode.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			e, err := domain.NewEdge(callerNode.ID, calleeNode.ID, domain.EdgeCalls,
				domain.WithConfidence(domain.Probable),
			)
			if err == nil {
				edges = append(edges, e)
			}
		}
	}
	return edges, unresolved
}

// extractReceiverBinding returns the receiver's parameter name and type
// from a method_declaration receiver_node. For `func (s *Server) Foo()`,
// returns ("s", "Server"). Either may be empty (anonymous receiver, or
// no type identifier found); callers should skip when recvName is empty.
func extractReceiverBinding(receiverNode *sitter.Node, src []byte) (name, typ string) {
	typ = extractReceiverType(receiverNode, src)

	// Receiver is a parameter_list with a single parameter_declaration.
	// The parameter_declaration has 'name' (identifier) + 'type'. Walk
	// for the first identifier under a parameter_declaration.
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if name != "" {
			return
		}
		if n.Type() == "parameter_declaration" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name = string(src[nameNode.StartByte():nameNode.EndByte()])
				return
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(receiverNode)
	return name, typ
}

// collectCallNames does a depth-first walk of node and returns the lookup
// keys for every call_expression we can resolve against the file's symbol
// map. Three forms are recognised:
//
//   - identifier call:        foo()             → "foo"
//   - Go selector call:       recvName.X()      → "recvType.X"   (only when
//     recvName and recvType are non-empty and the operand is an identifier
//     equal to recvName — Go uses selector_expression)
//   - TS member call:         this.X() / r.X()  → "recvType.X"   (only when
//     recvName and recvType are non-empty and the object text equals
//     recvName — TS/TSX tree-sitter uses member_expression with a "this"
//     literal child rather than an identifier, so matching by text covers
//     both r.X() with recvName="r" and this.X() with recvName="this".
//     solov2-gv6.)
//
// Cross-package selector calls (pkg.Bar(), or chained s.field.X())
// cannot be resolved without cross-repo type info, so they are skipped
// here. Adding them is solov2-1gj (cross-repo IMPORTS edges).
func collectCallNames(node *sitter.Node, src []byte, recvName, recvType string) []string {
	var names []string
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil {
				switch fn.Type() {
				case "identifier":
					names = append(names, string(src[fn.StartByte():fn.EndByte()]))
				case "selector_expression":
					if recvName != "" && recvType != "" {
						operand := fn.ChildByFieldName("operand")
						field := fn.ChildByFieldName("field")
						if operand != nil && field != nil &&
							operand.Type() == "identifier" &&
							string(src[operand.StartByte():operand.EndByte()]) == recvName {
							names = append(names, recvType+"."+string(src[field.StartByte():field.EndByte()]))
						}
					}
				case "member_expression":
					if recvName != "" && recvType != "" {
						object := fn.ChildByFieldName("object")
						property := fn.ChildByFieldName("property")
						if object != nil && property != nil &&
							string(src[object.StartByte():object.EndByte()]) == recvName {
							names = append(names, recvType+"."+string(src[property.StartByte():property.EndByte()]))
						}
					}
				}
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(node)
	return names
}

// ----- misc helpers -----

func extractPackageName(root *sitter.Node, src []byte) string {
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child.Type() == "package_clause" {
			nameNode := child.ChildByFieldName("name")
			if nameNode == nil {
				// fallback: second child (package keyword is first)
				if child.ChildCount() >= 2 {
					nameNode = child.Child(1)
				}
			}
			if nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
		}
	}
	return ""
}

func extractReceiverType(receiverNode *sitter.Node, src []byte) string {
	// receiver is a parameter_list: ( receiverSpec )
	// walk looking for a type_identifier or pointer_type -> type_identifier
	var walk func(*sitter.Node) string
	walk = func(n *sitter.Node) string {
		if n.Type() == "type_identifier" {
			return string(src[n.StartByte():n.EndByte()])
		}
		count := int(n.ChildCount())
		for i := range count {
			if result := walk(n.Child(i)); result != "" {
				return result
			}
		}
		return ""
	}
	return walk(receiverNode)
}

func extractSignature(node *sitter.Node, src []byte) string {
	params := node.ChildByFieldName("parameters")
	result := node.ChildByFieldName("result")

	if params == nil {
		return ""
	}

	var sb strings.Builder
	nameNode := node.ChildByFieldName("name")
	if nameNode != nil {
		sb.WriteString(string(src[nameNode.StartByte():nameNode.EndByte()]))
	}
	sb.WriteString(string(src[params.StartByte():params.EndByte()]))
	if result != nil {
		sb.WriteString(" ")
		sb.WriteString(string(src[result.StartByte():result.EndByte()]))
	}
	return sb.String()
}

func lineRange(node *sitter.Node) domain.LineRange {
	return domain.LineRange{
		Start: int(node.StartPoint().Row) + 1,
		End:   int(node.EndPoint().Row) + 1,
	}
}

func hasErrorNode(node *sitter.Node) bool {
	if node.IsError() || node.IsMissing() {
		return true
	}
	count := int(node.ChildCount())
	for i := range count {
		if hasErrorNode(node.Child(i)) {
			return true
		}
	}
	return false
}

// firstErrorFailure returns a ParseFailure describing the first ERROR or
// MISSING node found in a depth-first walk of the tree. If no such node is
// found (defensive — callers gate this with hasErrorNode), it returns a
// generic failure with Line 0.
func firstErrorFailure(node *sitter.Node) domain.ParseFailure {
	if node.IsError() {
		return domain.ParseFailure{
			Line:    int(node.StartPoint().Row) + 1,
			Message: "syntax error",
		}
	}
	if node.IsMissing() {
		return domain.ParseFailure{
			Line:    int(node.StartPoint().Row) + 1,
			Message: "missing token: " + node.Type(),
		}
	}
	count := int(node.ChildCount())
	for i := range count {
		child := node.Child(i)
		if hasErrorNode(child) {
			return firstErrorFailure(child)
		}
	}
	return domain.ParseFailure{Message: "syntax error"}
}

// nodeID produces a deterministic identifier for a node.
func nodeID(repoID, path string, kind domain.NodeKind, name string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", repoID, path, string(kind), name)
	return hex.EncodeToString(h.Sum(nil))
}
