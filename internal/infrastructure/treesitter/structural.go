// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	sitter "github.com/smacker/go-tree-sitter"
)

// goIdentifierTypes maps the tree-sitter-go leaf node types that name a symbol,
// variable, field, type, package, or label. Normalizing them makes the structural
// hash invariant to variable renames.
var goIdentifierTypes = map[string]struct{}{
	"identifier":         {},
	"field_identifier":   {},
	"type_identifier":    {},
	"package_identifier": {},
	"label_name":         {},
}

// goLiteralClass collapses literal types to representative class tokens (for example,
// `$NUM` for numeric literals), ensuring different literal values do not block a
// structural match.
var goLiteralClass = map[string]string{
	"int_literal":                "$NUM",
	"float_literal":              "$NUM",
	"imaginary_literal":          "$NUM",
	"rune_literal":               "$CHAR",
	"interpreted_string_literal": "$STR",
	"raw_string_literal":         "$STR",
}

// goStructuralHash returns a SHA-256 hash over a normalized token stream of the
// declaration. Identifiers are renamed consistently based on their order of appearance,
// ignoring comments and whitespace, to identify Type-2 clones.
func goStructuralHash(decl *sitter.Node, src []byte) string {
	h := sha256.New()
	idMap := make(map[string]string)
	sep := []byte{0}

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		t := n.Type()
		if t == "comment" {
			return
		}
		if cls, ok := goLiteralClass[t]; ok {
			// Treat the whole literal as one class token; don't recurse into
			// its quote/content tokens (string literals have children).
			h.Write([]byte(cls))
			h.Write(sep)
			return
		}
		if n.ChildCount() == 0 {
			if _, isID := goIdentifierTypes[t]; isID {
				name := string(src[n.StartByte():n.EndByte()])
				ph, ok := idMap[name]
				if !ok {
					ph = "$" + strconv.Itoa(len(idMap)+1)
					idMap[name] = ph
				}
				h.Write([]byte(ph))
			} else {
				// keyword / operator / punctuation - a structural token.
				h.Write([]byte(t))
			}
			h.Write(sep)
			return
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(decl)
	return hex.EncodeToString(h.Sum(nil))
}
