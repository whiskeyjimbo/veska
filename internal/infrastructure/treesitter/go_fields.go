package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// This file implements the struct field type analysis used to resolve chained selector
// method calls like `s.field.Method()`. The call extraction logic itself resides in go.go.

// fieldType describes a struct field's declared type. It stores the base type name
// and package qualifier with pointer, slice, and array modifiers stripped.
type fieldType struct {
	pkg  string // empty for fields declared with a local type name
	name string // base type name with *,, chan etc. stripped
}

// collectStructFields extracts field types for all top-level struct declarations in a file.
// The returned map is used to resolve method calls on struct fields.
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

// extractStructFields parses field declarations from a struct body. Multiple fields
// sharing a single type definition (for example, `a, b int`) each map to their
// shared type.
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
				// Tree-sitter Go grammar exposes fields under the name field when singular, or as
				// sibling children of type field_identifier when multiple fields are declared together.
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

// classifyFieldType extracts the base type name and optional package qualifier from
// a field's type node, stripping pointer, slice, array, and channel modifiers.
// It returns false for unsupported types like functions, maps, or anonymous
// struct/interface literals.
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
		// in the s.field.M shape we resolve. Skip.
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
