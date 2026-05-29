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
			n, err := domain.NewNode(id, path, fullName, domain.KindMethod,
				domain.WithLanguage("go"),
				domain.WithLines(lr),
				domain.WithRawContent(raw),
				domain.WithExported(goExported(methodName)),
			)
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
// for (solov2-b7wt).

// ----- CALLS extraction -----

// callKeySep separates the parts of an in-file call-dedup key. A NUL byte
// cannot appear in a node id or identifier, so it is unambiguous and shared by
// both the resolved-edge (seen) and unresolved-call (seenU) maps (solov2-2efk).
const callKeySep = "\x00"

// Cross-package call handling (solov2-xc51): collectCallNames returns
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
// call_chain and blast_radius (solov2-kzxh).
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
// fieldType describes a struct field's declared type for the limited
// purpose of resolving chained-selector method calls `s.field.M()`. The
// parser strips pointer/slice/etc. modifiers and records the base type
// name plus an optional package qualifier (solov2-9rc2 phase E).
//
// Phase E v1 only acts on fields whose pkg=="" (same-package concrete
// types) — those bind directly to the method node already in the
// file's symbol map. Cross-package field types and interface-typed
// fields are captured but the resolver currently ignores them; they
// remain TODOs for phase E v2.
type fieldType struct {
	pkg  string // empty for fields declared with a local type name
	name string // base type name with *, [], chan etc. stripped
}

// collectStructFields walks every top-level type_declaration with a
// struct_type body and returns a map of struct-name -> field-name ->
// fieldType, keyed by the struct name as it appears in the type_spec
// (without any pointer/slice prefix). Used by collectCallNames to
// resolve `s.field.M()` chains where s is the method receiver.
func collectStructFields(root *sitter.Node, src []byte) map[string]map[string]fieldType {
	out := map[string]map[string]fieldType{}
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child.Type() != "type_declaration" {
			continue
		}
		specCount := int(child.ChildCount())
		for j := range specCount {
			spec := child.Child(j)
			if spec.Type() != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode == nil || typeNode == nil || typeNode.Type() != "struct_type" {
				continue
			}
			structName := string(src[nameNode.StartByte():nameNode.EndByte()])
			fields := extractStructFields(typeNode, src)
			if len(fields) > 0 {
				out[structName] = fields
			}
		}
	}
	return out
}

// extractStructFields parses a struct_type node's field_declaration
// children into a name -> fieldType map. Skips field declarations the
// parser can't represent cleanly (embedded types without a name, struct
// tags-only nodes, anonymous fields). Multiple names sharing one type
// (`a, b int`) each map to the same fieldType.
func extractStructFields(structNode *sitter.Node, src []byte) map[string]fieldType {
	out := map[string]fieldType{}
	// struct_type -> field_declaration_list -> field_declaration
	walkStructFields(structNode, src, out)
	return out
}

func walkStructFields(n *sitter.Node, src []byte, out map[string]fieldType) {
	if n == nil {
		return
	}
	if n.Type() == "field_declaration" {
		// field_declaration has 'name' (may be a comma list) and 'type'.
		typeNode := n.ChildByFieldName("type")
		if typeNode != nil {
			ft, ok := classifyFieldType(typeNode, src)
			if ok {
				// Field name nodes are the named children typed 'field_identifier'
				// before the type node. Tree-sitter exposes them under 'name'
				// when there is one and as separate named children otherwise.
				namedCount := int(n.NamedChildCount())
				for i := range namedCount {
					nc := n.NamedChild(i)
					if nc == nil {
						continue
					}
					if nc.Type() != "field_identifier" {
						continue
					}
					name := string(src[nc.StartByte():nc.EndByte()])
					out[name] = ft
				}
			}
		}
	}
	count := int(n.ChildCount())
	for i := range count {
		walkStructFields(n.Child(i), src, out)
	}
}

// classifyFieldType extracts the base type name + optional package
// qualifier from a struct field's type node. Pointer (`*T`), slice
// (`[]T`), and channel (`chan T`) prefixes are stripped — for the
// purpose of method-call resolution we just need the underlying type.
// Returns ok=false for shapes the resolver can't act on (function
// types, map types, anonymous struct/interface literals, ...).
func classifyFieldType(typeNode *sitter.Node, src []byte) (fieldType, bool) {
	switch typeNode.Type() {
	case "pointer_type":
		// pointer_type wraps a type child; recurse.
		if inner := typeNode.NamedChild(0); inner != nil {
			return classifyFieldType(inner, src)
		}
		return fieldType{}, false
	case "slice_type", "array_type", "channel_type":
		// A slice/array of T or channel of T isn't directly method-callable
		// in the s.field.M() shape we resolve. Skip.
		return fieldType{}, false
	case "type_identifier":
		return fieldType{name: string(src[typeNode.StartByte():typeNode.EndByte()])}, true
	case "qualified_type":
		// pkg.Type — operand is the pkg identifier, field-like node is the type name.
		var pkg, name string
		count := int(typeNode.NamedChildCount())
		for i := range count {
			c := typeNode.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "package_identifier":
				pkg = string(src[c.StartByte():c.EndByte()])
			case "type_identifier":
				name = string(src[c.StartByte():c.EndByte()])
			}
		}
		if name == "" {
			return fieldType{}, false
		}
		return fieldType{pkg: pkg, name: name}, true
	}
	return fieldType{}, false
}

type callRef struct {
	name string
	pkg  string
	// method marks a call site where the operand is a local variable
	// whose origin is the named package — `v := pkg.New(...); v.Method()`.
	// pkg holds the originating package qualifier, name holds the method
	// identifier. The receiver type is unknown to the parser; the
	// promotion-time resolver binds by method name within pkg. Plain
	// pkg.Foo() calls keep method=false (solov2-9rc2).
	method bool
	// line is the 1-indexed start line of the call_expression in the
	// source file. Carried through to domain.Edge.SourceLine on
	// resolved edges and domain.UnresolvedCall.SrcLine for the
	// promotion-time resolver, so cross-repo edge attribution reports
	// the actual call site instead of the caller node's declaration
	// line (solov2-izh6.31). 0 = unknown.
	line int
}

// localVarOrigin tracks variables declared via `v := pkg.X(...)` inside
// a function body, mapping the local name to its origin package. Used
// to recognise chained-selector calls like `v.Method()` whose receiver
// type the parser cannot infer (solov2-9rc2). The map covers the most
// common Go DI pattern (constructor + method calls); more elaborate
// inference (var via assignment, method chains through interfaces) is
// out of scope here — those fall through to the existing unresolved
// path and stay unbound.
type localVarOrigin = string

// collectLocalVarOrigins walks a function body and returns the map of
// short-var-declared identifiers to their originating package qualifier.
// Recognised RHS shapes (see originPkgFromRHS):
//   - `v := pkg.X(...)`          call_expression with selector function
//   - `v := pkg.Type{...}`       composite literal with qualified type
//   - `v := &pkg.Type{...}`      address-of composite literal
//
// Unrecognised shapes (multi-value returns, type assertions, method
// chains, anonymous-type literals) are intentionally skipped so the map
// never contains a wrong origin — a missing entry just degrades to
// existing behaviour.
func collectLocalVarOrigins(node *sitter.Node, src []byte) map[string]localVarOrigin {
	origins := map[string]localVarOrigin{}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "short_var_declaration" {
			left := n.ChildByFieldName("left")
			right := n.ChildByFieldName("right")
			if left != nil && right != nil &&
				int(left.NamedChildCount()) == 1 &&
				int(right.NamedChildCount()) == 1 {
				lhs := left.NamedChild(0)
				rhs := right.NamedChild(0)
				if lhs != nil && rhs != nil && lhs.Type() == "identifier" {
					if pkg := originPkgFromRHS(rhs, src); pkg != "" {
						origins[string(src[lhs.StartByte():lhs.EndByte()])] = pkg
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
	return origins
}

// collectPackageVarOrigins walks the file root and returns origins for
// top-level `var x = ...` declarations whose RHS is a package-qualified
// constructor call or composite literal. Mirrors collectLocalVarOrigins
// but covers the file-scope shape that drives every cobra app:
//
//	var rootCmd = &cobra.Command{...}   // -> rootCmd -> cobra
//	var defaultPool = pool.New()        // -> defaultPool -> pool
//
// Before this collector landed, `rootCmd.AddCommand(helloCmd)` in init()
// emitted UnresolvedCall{PkgQualifier:"rootCmd"} — an unresolvable
// bareword — so no cross-repo CALLS stub was ever created against the
// cobra module (solov2-8ffo / solov2-zuvl, surfaced by the junior
// onboarding journey).
//
// Only single-spec `var x = expr` shapes are recognised; grouped
// `var ( ... )` blocks and multi-LHS specs are walked through the
// var_spec_list path so each spec is inspected individually.
func collectPackageVarOrigins(root *sitter.Node, src []byte) map[string]localVarOrigin {
	origins := map[string]localVarOrigin{}
	if root == nil {
		return origins
	}
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child == nil || child.Type() != "var_declaration" {
			continue
		}
		specCount := int(child.ChildCount())
		for j := range specCount {
			spec := child.Child(j)
			if spec == nil || spec.Type() != "var_spec" {
				continue
			}
			name := spec.ChildByFieldName("name")
			value := spec.ChildByFieldName("value")
			if name == nil || value == nil {
				continue
			}
			// Single-name, single-value only — multi-LHS and tuple
			// returns are intentionally skipped to keep origin
			// inferences unambiguous.
			if int(name.NamedChildCount()) > 1 || int(value.NamedChildCount()) != 1 {
				continue
			}
			id := name
			if id.Type() != "identifier" {
				id = name.NamedChild(0)
			}
			if id == nil || id.Type() != "identifier" {
				continue
			}
			rhs := value.NamedChild(0)
			if rhs == nil {
				continue
			}
			if pkg := originPkgFromRHS(rhs, src); pkg != "" {
				origins[string(src[id.StartByte():id.EndByte()])] = pkg
			}
		}
	}
	return origins
}

// collectInFileFunctionReturns walks the file root for top-level
// function declarations whose result is a single, simple type (or
// pointer to one) and returns funcName → bareReturnTypeName. Only
// shapes the cross-package-CALLS resolver can use downstream are
// included:
//
//	func New() Greeting       -> New -> Greeting
//	func New() *Greeting      -> New -> Greeting
//
// Multi-result returns, named result parameters with non-trivial
// types, and qualified types (pkg.Other) are intentionally skipped:
// without same-file binding the resolver has nothing to do, and
// recording the wrong type would invent false edges (solov2-rlfe).
func collectInFileFunctionReturns(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	if root == nil {
		return out
	}
	count := int(root.ChildCount())
	for i := range count {
		child := root.Child(i)
		if child == nil || child.Type() != "function_declaration" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		resultNode := child.ChildByFieldName("result")
		if nameNode == nil || resultNode == nil {
			continue
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		if t := simpleReturnTypeName(resultNode, src); t != "" {
			out[name] = t
		}
	}
	return out
}

// simpleReturnTypeName returns the bare type identifier for a Go
// function's result node when it is a single type — `Greeting`,
// `*Greeting`, or `pkg.T` (skipped: see collectInFileFunctionReturns).
// Returns "" for anything else (parameter_list with >1 entry, tuple
// returns, generics, channels, slices, maps, …) so that ambiguity is
// expressed as absence rather than a wrong binding.
func simpleReturnTypeName(result *sitter.Node, src []byte) string {
	switch result.Type() {
	case "type_identifier":
		return string(src[result.StartByte():result.EndByte()])
	case "pointer_type":
		// pointer_type's only child is the pointed-to type expression.
		// Recurse so `*Greeting` and `**T` both unwrap correctly.
		if int(result.NamedChildCount()) == 1 {
			return simpleReturnTypeName(result.NamedChild(0), src)
		}
		return ""
	case "parameter_list":
		// `func() T` parses as parameter_list with a single
		// parameter_declaration whose type is T.
		if int(result.NamedChildCount()) != 1 {
			return ""
		}
		spec := result.NamedChild(0)
		if spec == nil || spec.Type() != "parameter_declaration" {
			return ""
		}
		typ := spec.ChildByFieldName("type")
		if typ == nil {
			return ""
		}
		return simpleReturnTypeName(typ, src)
	}
	return ""
}

// collectLocalReceiverTypes walks a function body for `v := F(...)`
// short-var declarations whose RHS is a bare-identifier call to a
// function in funcReturns. The returned map binds v to the function's
// return type so v.Method() downstream resolves to ReceiverType.Method
// in this same file (solov2-rlfe). Without it, `g := New("x"); g.Render()`
// in greet_test.go emitted UnresolvedCall{PkgQualifier:"g"} — a bare
// local-var name promotion could never bind, so test files were
// invisible to blast/call_chain for in-repo methods.
func collectLocalReceiverTypes(body *sitter.Node, src []byte, funcReturns map[string]string) map[string]string {
	out := map[string]string{}
	if body == nil || len(funcReturns) == 0 {
		return out
	}
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n.Type() == "short_var_declaration" {
			left := n.ChildByFieldName("left")
			right := n.ChildByFieldName("right")
			if left != nil && right != nil &&
				int(left.NamedChildCount()) == 1 &&
				int(right.NamedChildCount()) == 1 {
				lhs := left.NamedChild(0)
				rhs := right.NamedChild(0)
				if lhs != nil && rhs != nil && lhs.Type() == "identifier" && rhs.Type() == "call_expression" {
					fn := rhs.ChildByFieldName("function")
					if fn != nil && fn.Type() == "identifier" {
						callee := string(src[fn.StartByte():fn.EndByte()])
						if recvType, ok := funcReturns[callee]; ok {
							out[string(src[lhs.StartByte():lhs.EndByte()])] = recvType
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
	walk(body)
	return out
}

// originPkgFromRHS returns the qualifying package identifier for a
// var declaration's RHS, or "" when the shape isn't recognised.
// Handles three shapes that all behave the same way for downstream
// cross-repo CALLS resolution: a constructor call, a value composite
// literal, or an address-of composite literal.
func originPkgFromRHS(rhs *sitter.Node, src []byte) string {
	if rhs == nil {
		return ""
	}
	switch rhs.Type() {
	case "call_expression":
		fn := rhs.ChildByFieldName("function")
		if fn == nil || fn.Type() != "selector_expression" {
			return ""
		}
		operand := fn.ChildByFieldName("operand")
		if operand == nil || operand.Type() != "identifier" {
			return ""
		}
		return string(src[operand.StartByte():operand.EndByte()])
	case "composite_literal":
		t := rhs.ChildByFieldName("type")
		if t == nil || t.Type() != "qualified_type" {
			return ""
		}
		pkg := t.ChildByFieldName("package")
		if pkg == nil || pkg.Type() != "package_identifier" {
			return ""
		}
		return string(src[pkg.StartByte():pkg.EndByte()])
	case "unary_expression":
		// &pkg.Type{...} — operand is a composite_literal.
		operand := rhs.ChildByFieldName("operand")
		if operand == nil {
			return ""
		}
		return originPkgFromRHS(operand, src)
	}
	return ""
}

// extractImports walks the file's import declarations and returns a map from
// the local package identifier to its import path. For an explicit alias
// (import foo "x/y") the key is the alias; otherwise it is the path's last
// segment (import "x/y" -> "y"), which equals the package name in the common
// case. Blank ("_") and dot (".") imports are skipped — neither yields a
// usable qualifier (solov2-xc51).
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
// tree-sitter's claim that the file has a syntax error (solov2-0kv6).
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
							// promotion via the import map (solov2-xc51). The
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
