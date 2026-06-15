package treesitter

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	sitter "github.com/smacker/go-tree-sitter"
)

// goIdentifierTypes are the tree-sitter-go leaf node types that NAME a symbol,
// variable, field, type, package, or label. Normalising them is what makes the
// structural hash invariant to renames — i.e. catches Type-2 clones that
// content_hash (byte-identical) misses.
var goIdentifierTypes = map[string]struct{}{
	"identifier":         {},
	"field_identifier":   {},
	"type_identifier":    {},
	"package_identifier": {},
	"label_name":         {},
}

// goLiteralClass collapses each tree-sitter-go literal type to one class token,
// so "1" vs "42" or "x" vs "y" strings don't block a structural match — while a
// literal stays distinct from an identifier (a number is not a variable).
var goLiteralClass = map[string]string{
	"int_literal":                "$NUM",
	"float_literal":              "$NUM",
	"imaginary_literal":          "$NUM",
	"rune_literal":               "$CHAR",
	"interpreted_string_literal": "$STR",
	"raw_string_literal":         "$STR",
}

// goStructuralHash returns a hex SHA-256 over decl's identifier-/literal-
// normalised token stream, so two declarations with the same SHAPE after a
// consistent renaming of identifiers (and any literals) hash identically —
// Type-2 clone detection. Comments and whitespace are ignored (it is
// token-stream based, not text based).
//
// Identifiers are renamed CONSISTENTLY: the first distinct name becomes $1, the
// next $2, and so on, so a variable reused keeps the same token — `a+a` and
// `b+b` match, but `a+b` does NOT match `a+a`. This includes the declaration's
// own name, so two identically-bodied, differently-named functions collide
// (the de-dupe signal we want). Operators, punctuation, and keywords are
// emitted verbatim via their tree-sitter Type(), which carries the structure.
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
				// keyword / operator / punctuation — a structural token.
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
