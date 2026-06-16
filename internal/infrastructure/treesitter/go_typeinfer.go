package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// This file holds the variable-origin / return-type inference used to bind
// chained-selector and local-variable method calls (`v:= pkg.New; v.M` and
// `g:= New; g.Render`) at promotion time. The collectors here feed
// collectCallNames and the cross-package CALLS resolver; the call extraction
// itself lives in go.go.

// localVarOrigin tracks variables declared via `v:= pkg.X(.)` inside
// a function body, mapping the local name to its origin package. Used
// to recognise chained-selector calls like `v.Method` whose receiver
// type the parser cannot infer. The map covers the most
// common Go DI pattern (constructor + method calls); more elaborate
// inference (var via assignment, method chains through interfaces) is
// out of scope here — those fall through to the existing unresolved
// path and stay unbound.
type localVarOrigin = string

// collectLocalVarOrigins walks a function body and returns the map of
// short-var-declared identifiers to their originating package qualifier.
// Recognised RHS shapes (see originPkgFromRHS):
//
//	`v:= pkg.X(.)` call_expression with selector function
//	`v:= pkg.Type{.}` composite literal with qualified type
//	`v:= &pkg.Type{.}` address-of composite literal
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
// top-level `var x =.` declarations whose RHS is a package-qualified
// constructor call or composite literal. Mirrors collectLocalVarOrigins
// but covers the file-scope shape that drives every cobra app:
//
//	var rootCmd = &cobra.Command{.} // -> rootCmd -> cobra
//	var defaultPool = pool.New // -> defaultPool -> pool
//
// Before this collector landed, `rootCmd.AddCommand(helloCmd)` in init
// emitted UnresolvedCall{PkgQualifier:"rootCmd"} — an unresolvable
// bareword — so no cross-repo CALLS stub was ever created against the
// cobra module ( /, surfaced by the junior
// onboarding journey).
// Only single-spec `var x = expr` shapes are recognised; grouped
// `var (. )` blocks and multi-LHS specs are walked through the
// var_spec_list path so each spec is inspected individually.
// Verbatim relocation: this var-spec walk predates the per-function complexity
// gate, which is diff-scoped and only flags it because the file split makes git
// see the move as new code.
//
//nolint:cyclop // see note above: verbatim relocation, diff-scoped gate
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
//	func New Greeting -> New -> Greeting
//	func New *Greeting -> New -> Greeting
//
// Multi-result returns, named result parameters with non-trivial
// types, and qualified types (pkg.Other) are intentionally skipped:
// without same-file binding the resolver has nothing to do, and
// recording the wrong type would invent false edges.
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
		// `func T` parses as parameter_list with a single
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

// collectLocalReceiverTypes walks a function body for `v:= F(.)`
// short-var declarations whose RHS is a bare-identifier call to a
// function in funcReturns. The returned map binds v to the function's
// return type so v.Method downstream resolves to ReceiverType.Method
// in this same file. Without it, `g:= New("x"); g.Render`
// in greet_test.go emitted UnresolvedCall{PkgQualifier:"g"} — a bare
// local-var name promotion could never bind, so test files were
// invisible to blast/call_chain for in-repo methods.
// Verbatim relocation: this short-var walk predates the per-function complexity
// gate, which is diff-scoped and only flags it because the file split makes git
// see the move as new code.
//
//nolint:cyclop // see note above: verbatim relocation, diff-scoped gate
func collectLocalReceiverTypes(body *sitter.Node, src []byte, funcReturns map[string]string) map[string]string {
	out := map[string]string{}
	if body == nil {
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
				if lhs != nil && rhs != nil && lhs.Type() == "identifier" {
					name := string(src[lhs.StartByte():lhs.EndByte()])
					switch {
					case rhs.Type() == "call_expression":
						// `v:= F(.)` where F is a same-file function returning
						// a same-package type.
						fn := rhs.ChildByFieldName("function")
						if fn != nil && fn.Type() == "identifier" {
							if recvType, ok := funcReturns[string(src[fn.StartByte():fn.EndByte()])]; ok {
								out[name] = recvType
							}
						}
					default:
						// `v:= T{.}` / `v:= &T{.}` composite literal of a
						// bare same-package type.
						if t := compositeLitBareType(rhs, src); t != "" {
							out[name] = t
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

// compositeLitBareType returns the bare (unqualified, pointer-stripped) type
// name of a composite-literal RHS — `T{.}` or `&T{.}` — when the type is a
// same-package type_identifier, else "". A qualified `pkg.T{.}` returns ""
// (it is a cross-package value, handled by collectLocalVarOrigins as a package
// origin, not a same-package receiver type), so we never mis-bind `v.M` to a
// same-package `T.M` that isn't the real type.
func compositeLitBareType(rhs *sitter.Node, src []byte) string {
	switch rhs.Type() {
	case "composite_literal":
		t := rhs.ChildByFieldName("type")
		if t != nil && t.Type() == "type_identifier" {
			return string(src[t.StartByte():t.EndByte()])
		}
	case "unary_expression":
		// &T{.} — operand is the composite_literal.
		if op := rhs.ChildByFieldName("operand"); op != nil {
			return compositeLitBareType(op, src)
		}
	}
	return ""
}

// collectParamReceiverTypes maps the caller declaration's parameter names to
// their bare same-package type — `func f(o Order, s *Server)` yields
// {o:Order, s:Server} — so a method call on a typed parameter, `o.Method`,
// binds to `Order.Method` rather than falling through as an unbindable
// package-qualifier UnresolvedCall. Only bare `T` / `*T`
// parameter types are recorded (via simpleReturnTypeName, which pointer-strips
// to match the method node's receiver-type name); qualified, slice, map, func
// and generic parameter types yield "" and are skipped, so a wrong type is
// never recorded (a missing entry just degrades to prior behaviour).
func collectParamReceiverTypes(decl *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	if decl == nil {
		return out
	}
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return out
	}
	count := int(params.NamedChildCount())
	for i := range count {
		pd := params.NamedChild(i)
		if pd == nil || pd.Type() != "parameter_declaration" {
			continue
		}
		typeNode := pd.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		bare := simpleReturnTypeName(typeNode, src)
		if bare == "" {
			continue
		}
		// A parameter_declaration may share one type across several names:
		// `func f(a, b Order)`. Bind each identifier child to the bare type.
		nc := int(pd.NamedChildCount())
		for j := range nc {
			ch := pd.NamedChild(j)
			if ch != nil && ch.Type() == "identifier" {
				out[string(src[ch.StartByte():ch.EndByte()])] = bare
			}
		}
	}
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
		// &pkg.Type{.} — operand is a composite_literal.
		operand := rhs.ChildByFieldName("operand")
		if operand == nil {
			return ""
		}
		return originPkgFromRHS(operand, src)
	}
	return ""
}
