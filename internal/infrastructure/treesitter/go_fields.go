package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// This file holds the struct-field-type analysis used by collectCallNames to
// resolve chained-selector method calls `s.field.M()`: the fieldType record and
// the walkers that build a struct-name → field-name → fieldType map. The
// call-extraction and node-building helpers live in go.go.

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
