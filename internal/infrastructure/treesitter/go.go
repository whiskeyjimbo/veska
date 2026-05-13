package treesitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

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
		return &domain.ParseResult{}, nil
	}
	defer tree.Close()

	root := tree.RootNode()

	// If the root itself has errors, bail out gracefully.
	if hasErrorNode(root) {
		return &domain.ParseResult{}, nil
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
	callEdges := extractCallEdges(root, src, symbolByName)
	result.Edges = append(result.Edges, callEdges...)

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
func extractCallEdges(root *sitter.Node, src []byte, symbols map[string]*domain.Node) []*domain.Edge {
	var edges []*domain.Edge

	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		var callerNode *domain.Node

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
			rt := extractReceiverType(receiverNode, src)
			name := rt + "." + string(src[nameNode.StartByte():nameNode.EndByte()])
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

		callNames := collectCallNames(bodyNode, src)
		for _, callee := range callNames {
			calleeNode, ok := symbols[callee]
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
	}
	return edges
}

// collectCallNames does a depth-first walk of node and returns the names of all
// call_expression targets that are plain identifiers (i.e. same-package calls).
func collectCallNames(node *sitter.Node, src []byte) []string {
	var names []string
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil && fn.Type() == "identifier" {
				names = append(names, string(src[fn.StartByte():fn.EndByte()]))
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

// nodeID produces a deterministic identifier for a node.
func nodeID(repoID, path string, kind domain.NodeKind, name string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", repoID, path, string(kind), name)
	return hex.EncodeToString(h.Sum(nil))
}
