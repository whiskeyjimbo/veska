// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	goparser "go/parser"
	goscanner "go/scanner"
	gotoken "go/token"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// SupportedExtensions returns the file extensions supported by GoParser. The cold
// scan uses this list directly to filter files during discovery, avoiding duplicated
// extension lists across the codebase.
func (p *GoParser) SupportedExtensions() []string { return []string{".go"} }

// goExported reports whether a Go identifier is exported (its first rune is an
// uppercase letter). Callers must pass only the method name segment when checking
// compound names like "Receiver.Method".
func goExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

// This file contains structural AST-walking helpers that complement the query-driven
// parser, handling complex patterns like receiver binding and selector type analysis
// that cannot be easily expressed via tree-sitter query patterns.

// parseInterfaceMethods extracts method nodes from an interface declaration. Each
// node is named "IfaceName.MethodName" to match the naming convention of concrete
// methods. Embedded interfaces are skipped to avoid complex inheritance edge modeling.
func parseInterfaceMethods(typeDeclNode *sitter.Node, src []byte, repoID, path, ifaceName string) []*domain.Node {
	var out []*domain.Node
	// type_declaration -> type_spec -> type (interface_type) -> method_specs
	specCount := int(typeDeclNode.ChildCount())
	for i := range specCount {
		spec := typeDeclNode.Child(i)
		if spec.Type() != "type_spec" {
			continue
		}
		typeNode := spec.ChildByFieldName("type")
		if typeNode == nil || typeNode.Type() != "interface_type" {
			continue
		}
		bodyCount := int(typeNode.ChildCount())
		for j := range bodyCount {
			c := typeNode.Child(j)
			// Tree-sitter Go grammar emits 'method_elem' for each interface
			// method (older versions used 'method_spec'). The method name
			// is the first field_identifier child rather than a 'name'
			// field, so look it up by type rather than by ChildByFieldName.
			if c.Type() != "method_elem" && c.Type() != "method_spec" {
				continue
			}
			var nameNode *sitter.Node
			elemCount := int(c.ChildCount())
			for k := range elemCount {
				cc := c.Child(k)
				if cc.Type() == "field_identifier" || cc.Type() == "identifier" {
					nameNode = cc
					break
				}
			}
			if nameNode == nil {
				continue
			}
			methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
			fullName := ifaceName + "." + methodName
			id := nodeID(repoID, path, domain.KindMethod, fullName)
			lr := lineRange(c)
			raw := string(src[c.StartByte():c.EndByte()])
			n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: fullName, Kind: domain.KindMethod}, domain.WithLanguage("go"), domain.WithLines(lr), domain.WithRawContent(raw), domain.WithExported(goExported(methodName)))
			if err != nil {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// callKeySep separates the parts of an in-file call-deduplication key. A NUL byte
// is used as it cannot appear in standard identifiers or node IDs.
const callKeySep = "\x00"

// extractReceiverBinding returns the parameter name and type of a method declaration
// receiver (for example, "s" and "Server" for `func (s *Server) Foo`). Callers
// should check if the returned receiver name is empty.
func extractReceiverBinding(receiverNode *sitter.Node, src []byte) (name, typ string) {
	typ = extractReceiverType(receiverNode, src)

	// The receiver is represented as a parameter list containing a single parameter
	// declaration. We walk the tree to find its first identifier.
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

// collectCallNames extracts call references within a node. It matches local identifier
// calls, receiver method calls (resolved using the receiver type), and TypeScript
// member calls (which use member_expression with a literal 'this' child).
type callRef struct {
	name string
	pkg  string
	// method indicates if the call is on a local variable initialized from an
	// imported package (for example, `v := pkg.New(); v.Method()`).
	method bool
	// line is the 1-indexed start line of the call site, used to attribute cross-repo
	// edges to their precise call locations.
	line int
}

// extractImports maps local package aliases or default names to their import paths.
// Blank ("_") and dot (".") imports are omitted as they do not provide usable qualifiers.
func extractImports(root *sitter.Node, src []byte) map[string]string {
	imports := map[string]string{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "import_spec" {
			pathNode := n.ChildByFieldName("path")
			if pathNode != nil {
				path := strings.Trim(string(src[pathNode.StartByte():pathNode.EndByte()]), `"`)
				if path != "" {
					local := ""
					if nameNode := n.ChildByFieldName("name"); nameNode != nil {
						local = string(src[nameNode.StartByte():nameNode.EndByte()])
					}
					switch local {
					case "_", ".":
						// no usable qualifier
					case "":
						imports[lastPathSegment(path)] = path
					default:
						imports[local] = path
					}
				}
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(imports) == 0 {
		return nil
	}
	return imports
}

// lastPathSegment returns the final "/"-separated segment of an import path.
func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// misc helpers

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

// goParserCheck validates syntax errors reported by tree-sitter against the standard
// library's go/parser. It returns true if go/parser also rejects the file, which
// provides a more precise error message. If go/parser accepts the file, the
// tree-sitter error is a false positive (which can happen when the tree-sitter
// grammar lags Go's language spec updates) and should be suppressed.
func goParserCheck(path string, src []byte) (domain.ParseFailure, bool) {
	fset := gotoken.NewFileSet()
	_, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
	if err == nil {
		return domain.ParseFailure{}, false
	}
	// go/parser returns a scanner.ErrorList (*scanner.Error) for syntax
	// errors; pull the earliest position+message for a precise finding.
	if list, ok := err.(goscanner.ErrorList); ok && len(list) > 0 {
		first := list[0]
		return domain.ParseFailure{
			Line:    first.Pos.Line,
			Message: first.Msg,
		}, true
	}
	return domain.ParseFailure{Message: err.Error()}, true
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

// firstErrorFailure returns a ParseFailure describing the first ERROR or MISSING
// node found in a depth-first walk of the AST.
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

// collectCallNames extracts lookup keys for calls inside a function or method body.
// This legacy function is preserved for the TypeScript parser in ts.go, which still
// uses it for class-method call extraction.
func collectCallNames(node *sitter.Node, src []byte, recvName, recvType string, structFields map[string]map[string]fieldType) []callRef {
	// We pre-scan local variable initializations to trace methods called on variables
	// (for example, `v := pkg.New(); v.X()`) back to their originating package rather
	// than dropping them during promotion.
	localOrigins := collectLocalVarOrigins(node, src)
	var refs []callRef
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "call_expression" {
			fn := n.ChildByFieldName("function")
			if fn != nil {
				switch fn.Type() {
				case "identifier":
					refs = append(refs, callRef{name: string(src[fn.StartByte():fn.EndByte()])})
				case "selector_expression":
					operand := fn.ChildByFieldName("operand")
					field := fn.ChildByFieldName("field")
					// For chained selectors (e.g. `recvName.field.Method`), we resolve local field
					// types directly or emit cross-package method calls for imported types, which
					// are resolved during promotion.
					if operand != nil && field != nil && operand.Type() == "selector_expression" &&
						recvName != "" && recvType != "" {
						innerOperand := operand.ChildByFieldName("operand")
						innerField := operand.ChildByFieldName("field")
						if innerOperand != nil && innerField != nil && innerOperand.Type() == "identifier" {
							innerOp := string(src[innerOperand.StartByte():innerOperand.EndByte()])
							innerFld := string(src[innerField.StartByte():innerField.EndByte()])
							if innerOp == recvName {
								if fields, ok := structFields[recvType]; ok {
									if ft, ok := fields[innerFld]; ok {
										methodName := string(src[field.StartByte():field.EndByte()])
										if ft.pkg == "" {
											// If the field is defined in the same package, we resolve its method locally.
											refs = append(refs, callRef{name: ft.name + "." + methodName})
										} else {
											// If the field type belongs to an imported package, we emit a cross-package
											// method call reference.
											refs = append(refs, callRef{name: methodName, pkg: ft.pkg, method: true})
										}
									}
								}
							}
						}
					}
					if operand != nil && field != nil && operand.Type() == "identifier" {
						op := string(src[operand.StartByte():operand.EndByte()])
						fld := string(src[field.StartByte():field.EndByte()])
						switch {
						case recvName != "" && recvType != "" && op == recvName:
							// Resolve method receiver calls (for example, `s.Foo` inside a method on
							// `*Server` resolves to `Server.Foo`).
							refs = append(refs, callRef{name: recvType + "." + fld})
						case localOrigins[op] != "":
							// Resolve calls on initialized local variables (for example, `v.Method`
							// where `v := pkg.New()` resolves to `pkg.Method`).
							refs = append(refs, callRef{name: fld, pkg: localOrigins[op], method: true})
						default:
							// Resolve package-qualified function calls (for example, `pkg.Foo`), which
							// promotion will map to their imported package paths.
							refs = append(refs, callRef{name: fld, pkg: op})
						}
					}
				case "member_expression":
					if recvName != "" && recvType != "" {
						object := fn.ChildByFieldName("object")
						property := fn.ChildByFieldName("property")
						if object != nil && property != nil &&
							string(src[object.StartByte():object.EndByte()]) == recvName {
							refs = append(refs, callRef{name: recvType + "." + string(src[property.StartByte():property.EndByte()])})
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
	return refs
}

// buildPackageNode extracts the package name from a Go file's root AST node and
// builds a package node.
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
		// Package node intentionally omits Lines (matches the historical
		// extractPackageName + NewNode-without-WithLines behaviour).
		n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: domain.KindPackage}, domain.WithLanguage("go"))
		if err != nil {
			return nil
		}
		return n
	}
	return nil
}
