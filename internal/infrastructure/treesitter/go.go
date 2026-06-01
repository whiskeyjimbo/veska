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

// goExported reports whether a Go identifier is exported — its first rune is an
// uppercase letter. Names like "Receiver.Method" should be passed as the bare
// method-name segment by the caller.
func goExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

// GoParser, NewGoParser, and ParseFile live in go_query.go now (solov2-1yev
// phase 5). What remains in this file are the helpers that the query-driven
// parser still uses: receiver-binding extractors, struct-field-type analysis
// for chained selectors, the local-var-origin scanner, the import-map
// reader, and small utilities (signature/lineRange/hasErrorNode/nodeID/
// goExported/parseInterfaceMethods). Each survives because the query path
// is a thin shell over `runQuery` plus these structural walks — replacing
// them with .scm queries would either fight tree-sitter's predicate model
// (export-flag propagation, ERROR-node skip) or duplicate work the
// existing helpers do correctly. The legacy hand-rolled extractors
// (parseFunctionDecl / extractCallEdges / collectAnonCalls / ...) were
// deleted in this commit.

// ----- node extraction helpers -----

// parseInterfaceMethods walks an interface_type's method_spec children
// and returns one KindMethod node per declared method. Each node is
// named `IfaceName.MethodName` so it shares a key space with concrete
// methods (parseMethodDecl names methods the same way). Embedded
// interfaces (which appear as type_identifier under the interface body)
// are skipped here — capturing them as inheritance would require a
// new edge kind; the simpler win is just listing the declared methods
// (solov2-9rc2 phase E v2).
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

// parseTopLevelVarDecl extracts every name declared by a top-level
// var_declaration or const_declaration as a KindVariable node. Tree-sitter's
// grammar nests one or more var_spec / const_spec children inside the
// declaration; each spec may itself bind multiple identifiers
// (`var a, b = 1, 2`). Underscore names (`_`) are skipped — they aren't
// addressable.
//
// Captured for framework-config patterns where the API surface lives in
// initialised package-scope vars: cobra `var rootCmd = &cobra.Command{...}`,
// gin/echo router globals, viper config singletons. Without this pass the
// graph misses the entire command tree of any cobra-based CLI and
// eng_find_symbol returns empty for the var names users actually search
// for .

// ----- CALLS extraction -----

// callKeySep separates the parts of an in-file call-dedup key. A NUL byte
// cannot appear in a node id or identifier, so it is unambiguous and shared by
// both the resolved-edge (seen) and unresolved-call (seenU) maps .
const callKeySep = "\x00"

// Cross-package call handling : collectCallNames returns
// package-qualified calls (pkg.Bar()) with callRef.pkg set. extractCallEdges
// cannot bind them in-file, so it stashes them as UnresolvedCalls carrying the
// qualifier; the promotion store resolves each against the file's import map —
// to a concrete CALLS edge for intra-module targets, or a cross-repo edge stub
// for external modules (which the query-time resolver later binds, solov2-1gj).

// extractCallEdges walks the entire AST looking for call_expression nodes inside
// function/method bodies and emits EdgeCalls when the callee is known in the file.

// extractTopLevelVarInitCalls walks top-level var_declaration and const_declaration
// children, finds function_literal bodies anywhere inside them, and emits CALLS
// edges from pkgNode (the file's package node) to every callable target in those
// bodies. This makes cobra-style anonymous-function call patterns visible to
// call_chain and blast_radius .
//
// Only identifier-form calls are bound here; package-qualified and selector

// collectAnonCalls walks node looking for function_literal subtrees; for each
// one it harvests identifier and package-qualified calls in the body and
// attributes them to callerNode. Recursive so nested closures
// (func(){ go func(){ Foo() }() }) are reached too.

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
// Package-qualified selector calls (pkg.Bar()) are returned with callRef.pkg
// set so promotion can resolve them via the import map (see the cross-package
// note on extractCallEdges, solov2-xc51). Chained selectors (s.field.X()) whose
// operand is not a plain identifier are still skipped.
// callRef is one call site collected from a function/method body. name is the
// callee identifier (or "Receiver.method" for a resolved receiver call); pkg is
// the selector operand for a package-qualified call (the "cmd" in cmd.Execute()),
// empty for plain or receiver-local calls.
type callRef struct {
	name string
	pkg  string
	// method marks a call site where the operand is a local variable
	// whose origin is the named package — `v := pkg.New(...); v.Method()`.
	// pkg holds the originating package qualifier, name holds the method
	// identifier. The receiver type is unknown to the parser; the
	// promotion-time resolver binds by method name within pkg. Plain
	// pkg.Foo() calls keep method=false .
	method bool
	// line is the 1-indexed start line of the call_expression in the
	// source file. Carried through to domain.Edge.SourceLine on
	// resolved edges and domain.UnresolvedCall.SrcLine for the
	// promotion-time resolver, so cross-repo edge attribution reports
	// the actual call site instead of the caller node's declaration
	// line (solov2-izh6.31). 0 = unknown.
	line int
}

// extractImports walks the file's import declarations and returns a map from
// the local package identifier to its import path. For an explicit alias
// (import foo "x/y") the key is the alias; otherwise it is the path's last
// segment (import "x/y" -> "y"), which equals the package name in the common
// case. Blank ("_") and dot (".") imports are skipped — neither yields a
// usable qualifier .
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

// ----- misc helpers -----

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

// goParserCheck re-parses src with the standard library go/parser to validate
// tree-sitter's claim that the file has a syntax error .
//
// Returns (ParseFailure, true) when go/parser ALSO rejects the file — in
// that case the failure carries go/parser's line + first error message
// (more precise than tree-sitter's generic "syntax error").
//
// Returns (_, false) when go/parser accepts the file — the tree-sitter
// error is a false positive (the smacker grammar lags Go's spec; e.g. it
// chokes on Go 1.26+ `new("string-literal")` conversions). Callers should
// suppress the parse-failure finding in that case.
func goParserCheck(path string, src []byte) (domain.ParseFailure, bool) {
	fset := gotoken.NewFileSet()
	_, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
	if err == nil {
		return domain.ParseFailure{}, false
	}
	// go/parser returns a scanner.ErrorList ([]*scanner.Error) for syntax
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

// collectCallNames is the legacy call-name extractor. solov2-1yev phase 5
// deleted every other recursive walker in this file in favour of the
// query-driven Go parser (go_query.go) — collectCallNames survives because
// ts.go (the TypeScript parser, not yet ported to queries) still depends
// on it for class-method body call extraction. When solov2-brw6 lands
// the TS port + the unified-binary refactor, this function moves with it
// (or gets replaced by ts-side queries) and goes away here.
func collectCallNames(node *sitter.Node, src []byte, recvName, recvType string, structFields map[string]map[string]fieldType) []callRef {
	// solov2-9rc2: pre-scan local-var origins so `v := pkg.New(...); v.X()`
	// is recognised as a method call on a value from pkg instead of being
	// silently dropped (the old branch treated the operand as if it were
	// an import qualifier and lost the call at promotion).
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
					// solov2-9rc2 phase E: chained selector recvName.field.Method.
					// v1 handles same-package field types (local symbol-map
					// lookup); v2 emits a cross-package method-call ref so
					// promotion's existing Phase C/D machinery binds it
					// against interface/struct method nodes in the imported
					// package.
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
											// v1: same package. Local symbol-map
											// lookup will find FieldType.Method
											// directly (struct or interface method
											// nodes both live under that key).
											refs = append(refs, callRef{name: ft.name + "." + methodName})
										} else {
											// v2: cross-package. Emit a method
											// call with the field's package as
											// the qualifier; Phase C+D handle
											// the rest (method-call cross-repo
											// stub, suffix-match resolution).
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
							// s.foo() inside a method on *Server -> Server.foo (local).
							refs = append(refs, callRef{name: recvType + "." + fld})
						case localOrigins[op] != "":
							// v.Method() where v := pkg.New(...). solov2-9rc2:
							// emit as a method call referencing the originating
							// package; the promotion-time resolver binds by name
							// within that package.
							refs = append(refs, callRef{name: fld, pkg: localOrigins[op], method: true})
						default:
							// pkg.Foo() — package-qualified; resolved at
							// promotion via the import map . The
							// operand may also be a local variable; a
							// non-import qualifier simply misses there.
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
