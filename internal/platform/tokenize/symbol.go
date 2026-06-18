// Package tokenize provides pure-Go helpers that pre-tokenise symbol-bearing
// strings (kind, symbol_path, name) into a whitespace-joined form suitable
// for indexing by FTS5's built-in unicode61 tokenizer.
// Why pre-tokenise in Go rather than register a custom FTS5 tokenizer?
// Custom tokenizers are platform-specific and brittle to wire through
// database/sql. Pre-tokenising on the write path lets unicode61 do what
// it already does well - lower-case folding, diacritic stripping,
// latin-script word splitting - while the upstream Go code is responsible
// for the splits unicode61 cannot make (camelCase, snake_case, `::`, `.`).
package tokenize

import (
	"strings"
	"unicode"
)

// Symbol splits text into tokens by:
//
//	any rune that is not a letter or digit (covers `.`, `::`, `/`, `-`,
//	  whitespace, `_`, etc.);
//	lower→upper transitions inside an identifier (camelCase: "closeFinding"
//	  → ["close", "Finding"]);
//	end-of-acronym → next word boundaries (ACRONYM-style: "HTTPServer" →
//	  ["HTTP", "Server"]).
//
// The result is whitespace-joined: the original text is preserved as one
// "atom" by also being emitted first, followed by the per-token splits.
// FTS5 unicode61 will then lower-case-fold the whole thing.
// Empty input returns "". The output never carries leading or trailing
// whitespace and never contains consecutive spaces.
func Symbol(text string) string {
	if text == "" {
		return ""
	}

	var out strings.Builder
	out.Grow(len(text) * 2)

	// First, emit the whole input as one chunk with non-alnum runes
	// converted to spaces. This preserves the "raw" string in a form
	// that unicode61 can tokenise - e.g. "pkg/api.closeFinding" →
	// "pkg api closeFinding".
	flattened := flattenNonAlnum(text)
	out.WriteString(flattened)

	// Then, expand each space-separated chunk by camelCase/acronym
	// splitting and append those expansions.
	for chunk := range strings.FieldsSeq(flattened) {
		parts := splitCamel(chunk)
		// Only append the parts if there are at least two - otherwise the
		// chunk is already a single word and would just be duplicated.
		if len(parts) >= 2 {
			for _, p := range parts {
				out.WriteByte(' ')
				out.WriteString(p)
			}
		}
	}

	return out.String()
}

// flattenNonAlnum returns text with every rune that is not a letter or digit
// replaced by a single space, with consecutive spaces collapsed and leading
// and trailing whitespace trimmed.
func flattenNonAlnum(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	prevSpace := true // suppress leading space
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			prevSpace = false
			continue
		}
		if !prevSpace {
			out.WriteByte(' ')
			prevSpace = true
		}
	}
	s := out.String()
	return strings.TrimRight(s, " ")
}

// splitCamel splits an identifier on camelCase and acronym boundaries.
// "closeFinding" -> ["close", "Finding"]
// "HTTPServer" -> ["HTTP", "Server"]
// "parseURL2Path" -> ["parse", "URL", "2", "Path"] (digits split too)
// "closefinding" -> ["closefinding"]
func splitCamel(s string) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		// boundary: lower|digit → Upper (closeFinding)
		if (unicode.IsLower(prev) || unicode.IsDigit(prev)) && unicode.IsUpper(cur) {
			parts = append(parts, string(runes[start:i]))
			start = i
			continue
		}
		// boundary: Upper Upper lower → acronym|Word (HTTPServer)
		if i+1 < len(runes) &&
			unicode.IsUpper(prev) && unicode.IsUpper(cur) && unicode.IsLower(runes[i+1]) {
			parts = append(parts, string(runes[start:i]))
			start = i
			continue
		}
		// boundary: letter → digit, digit → letter
		if (unicode.IsLetter(prev) && unicode.IsDigit(cur)) ||
			(unicode.IsDigit(prev) && unicode.IsLetter(cur)) {
			parts = append(parts, string(runes[start:i]))
			start = i
			continue
		}
	}
	parts = append(parts, string(runes[start:]))
	return parts
}
