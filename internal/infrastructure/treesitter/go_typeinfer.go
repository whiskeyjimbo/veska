package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// This file implements variable origin and return type inference to resolve method calls
// on variables initialized from constructor functions or composite literals. The extracted
// information is used during call resolution.

// localVarOrigin maps local variable names to their source packages (for example,
// mapping `v` to `pkg` for `v := pkg.New()`).
type localVarOrigin = string

// collectLocalVarOrigins maps variables declared with short variable assignments (`:=`)
// to their originating package. It recognizes constructor calls and composite literals,
// skipping unrecognized patterns to avoid incorrect inferences.
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

// collectPackageVarOrigins extracts origins for top-level variables initialized with
// package-qualified constructors or composite literals (for example, `var rootCmd = &cobra.Command{}`).
// Only single-variable declarations are processed.
//
//nolint:cyclop // We accept this cyclomatic complexity as the var spec walk logic is self-contained.
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

// collectInFileFunctionReturns maps local function names to their simple return type
// names (for example, mapping `New` to `Greeting` for `func New() *Greeting`). Multi-result
// and non-local types are skipped to avoid false positive bindings.
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

// simpleReturnTypeName returns the bare type name of a single return value, unwrapping
// pointer modifiers. It returns an empty string for complex types like maps, slices,
// channels, or multiple return values.
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

// collectLocalReceiverTypes matches variables initialized from local constructor
// functions in funcReturns and associates them with their return types to resolve
// method calls locally.
//
//nolint:cyclop // We accept this cyclomatic complexity as the short variable walk logic is self-contained.
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

// compositeLitBareType returns the unqualified base type name of a composite literal
// (for example, `T` for `&T{}`). It returns an empty string for qualified package types.
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

// collectParamReceiverTypes maps parameter names to their simple same-package types
// (for example, mapping `o` to `Order` in `func f(o Order)`). Complex and cross-package
// parameter types are omitted.
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
		// Bind each parameter identifier to the shared type when multiple names share a type
		// definition (for example, `func f(a, b Order)`).
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

// originPkgFromRHS extracts the package prefix from a package-qualified call or
// composite literal expression.
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

		operand := rhs.ChildByFieldName("operand")
		if operand == nil {
			return ""
		}
		return originPkgFromRHS(operand, src)
	}
	return ""
}
