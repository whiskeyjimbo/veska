// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// extractEmbeds returns the embed relationships declared by a struct or interface
// type. An embedded struct field is a field_declaration with a type but no field
// name; an embedded interface is a bare type reference inside an interface body.
// The embedded type feeds method promotion during IMPLEMENTS resolution, so we
// capture it even though it carries no field name.
func extractEmbeds(declNode *sitter.Node, src []byte, srcID domain.NodeID) []domain.UnresolvedTypeRel {
	typeNode := typeBodyOf(declNode)
	if typeNode == nil {
		return nil
	}
	switch typeNode.Type() {
	case "struct_type":
		return structEmbeds(typeNode, src, srcID)
	case "interface_type":
		return interfaceEmbeds(typeNode, src, srcID)
	}
	return nil
}

// typeBodyOf returns the struct_type/interface_type body of a type declaration.
func typeBodyOf(declNode *sitter.Node) *sitter.Node {
	count := int(declNode.ChildCount())
	for i := range count {
		spec := declNode.Child(i)
		if spec.Type() != "type_spec" {
			continue
		}
		if t := spec.ChildByFieldName("type"); t != nil {
			return t
		}
	}
	return nil
}

// structEmbeds finds field_declaration nodes that have a type but no field
// identifier - the Go grammar's representation of an embedded field.
func structEmbeds(structNode *sitter.Node, src []byte, srcID domain.NodeID) []domain.UnresolvedTypeRel {
	var out []domain.UnresolvedTypeRel
	var walk func(*sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "field_declaration" && !hasFieldName(n) {
			// An embedded pointer field (`*Base`) emits the `*` as a sibling token
			// rather than a pointer_type node, so detect it on the declaration.
			pointer := hasStarToken(n)
			typeNode := firstTypeChild(n)
			if rel, ok := embedRelFromType(typeNode, src, srcID, pointer); ok {
				out = append(out, rel)
			}
		}
		count := int(n.ChildCount())
		for i := range count {
			walk(n.Child(i))
		}
	}
	walk(structNode)
	return out
}

// interfaceEmbeds finds embedded interfaces: bare type references (type_identifier
// or qualified_type) inside the interface body that are not method elements.
func interfaceEmbeds(ifaceNode *sitter.Node, src []byte, srcID domain.NodeID) []domain.UnresolvedTypeRel {
	var out []domain.UnresolvedTypeRel
	count := int(ifaceNode.ChildCount())
	for i := range count {
		c := ifaceNode.Child(i)
		switch c.Type() {
		case "type_identifier", "qualified_type":
			if rel, ok := embedRelFromType(c, src, srcID, false); ok {
				out = append(out, rel)
			}
		case "type_elem":
			// Newer grammars wrap embedded interfaces (and constraint elements) in
			// a type_elem. Only a lone type reference is an embed; unions/approx
			// constraints are generics and out of scope.
			if c.NamedChildCount() == 1 {
				if rel, ok := embedRelFromType(c.NamedChild(0), src, srcID, false); ok {
					out = append(out, rel)
				}
			}
		}
	}
	return out
}

// hasFieldName reports whether a field_declaration declares at least one named
// field (vs. an embedded field, which has none).
func hasFieldName(fieldDecl *sitter.Node) bool {
	count := int(fieldDecl.NamedChildCount())
	for i := range count {
		if c := fieldDecl.NamedChild(i); c != nil && c.Type() == "field_identifier" {
			return true
		}
	}
	return false
}

// hasStarToken reports whether a declaration has a `*` token child, marking a
// pointer embed (`*Base`), which the grammar represents as a sibling token.
func hasStarToken(n *sitter.Node) bool {
	count := int(n.ChildCount())
	for i := range count {
		if c := n.Child(i); c != nil && c.Type() == "*" {
			return true
		}
	}
	return false
}

// firstTypeChild returns the first child that looks like a type reference, used
// when an embedded field's type is not exposed under the "type" field.
func firstTypeChild(n *sitter.Node) *sitter.Node {
	count := int(n.NamedChildCount())
	for i := range count {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "qualified_type", "pointer_type":
			return c
		}
	}
	return nil
}

// embedRelFromType builds an embed relationship from a type node, reusing the
// field-type classifier so pointer/qualifier handling matches the rest of the
// parser. It returns false for types that are not a plain (optionally pointer)
// named type, e.g. anonymous structs or generic instantiations.
func embedRelFromType(typeNode *sitter.Node, src []byte, srcID domain.NodeID, pointer bool) (domain.UnresolvedTypeRel, bool) {
	if typeNode == nil {
		return domain.UnresolvedTypeRel{}, false
	}
	// A pointer_type node (rare for embeds, but possible) also counts as a pointer.
	if typeNode.Type() == "pointer_type" {
		pointer = true
	}
	ft, ok := classifyFieldType(typeNode, src)
	if !ok || ft.name == "" {
		return domain.UnresolvedTypeRel{}, false
	}
	return domain.UnresolvedTypeRel{
		SrcID:        srcID,
		TargetName:   ft.name,
		PkgQualifier: ft.pkg,
		Kind:         domain.EdgeEmbeds,
		Pointer:      pointer,
		SrcLine:      int(typeNode.StartPoint().Row) + 1,
	}, true
}
