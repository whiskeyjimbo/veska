package treesitter

import (
	"bytes"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// todoMarkers is the closed set of marker tokens scanTodos recognises.
// We deliberately exclude generic words ("NOTE", "HACK") to keep the
// signal-to-noise ratio high — only actionable markers are surfaced.
var todoMarkers = []string{"TODO", "FIXME", "XXX"}

// scanTodos walks src once line-by-line and returns a ParseTodo for each
// line that begins (after optional comment leaders and whitespace) with
// one of the recognised markers.
//
// The scan is lexical and language-agnostic: it understands the common
// single-line and block-comment leaders //, #, /*, *, --, <!--, ; and any
// run of whitespace before the marker. It does NOT distinguish code from
// strings — a literal containing "// TODO" would be flagged. We accept
// this false-positive rate as the cost of language-independence: the
// alternative is a per-language tree-sitter query, which costs an order
// of magnitude more code for marginal precision.
//
// Each emitted ParseTodo.Line is 1-based. ParseTodo.Message is the
// substring of the line FROM the marker onward, with the trailing
// closing comment leader (*/ or -->) stripped and whitespace trimmed.
// The marker itself is included in the message so renderers can show
// "TODO: foo" vs "FIXME: bar" without an extra field.
func scanTodos(src []byte) []domain.ParseTodo {
	if len(src) == 0 {
		return nil
	}
	var out []domain.ParseTodo
	line := 1
	start := 0
	for i := 0; i <= len(src); i++ {
		if i == len(src) || src[i] == '\n' {
			if todo, ok := matchTodoLine(src[start:i], line); ok {
				out = append(out, todo)
			}
			line++
			start = i + 1
		}
	}
	return out
}

// matchTodoLine returns a populated ParseTodo when raw (one line, no
// trailing newline) starts with a recognised TODO marker after the
// optional comment-leader run.
func matchTodoLine(raw []byte, line int) (domain.ParseTodo, bool) {
	// Find where the comment content begins. We skip whitespace, then
	// known leader runs, then whitespace again. Repeating until stable
	// handles cases like "  // " and "/* TODO".
	i := 0
	for {
		j := i
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
			j++
		}
		k := j
		// Single-character leaders.
		for k < len(raw) && (raw[k] == '/' || raw[k] == '*' || raw[k] == '#' || raw[k] == '-' || raw[k] == ';' || raw[k] == '<' || raw[k] == '!') {
			k++
		}
		if k == j {
			i = j
			break
		}
		i = k
	}
	// Skip one more run of whitespace before the marker.
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
		i++
	}
	if i >= len(raw) {
		return domain.ParseTodo{}, false
	}

	rest := raw[i:]
	for _, m := range todoMarkers {
		if !bytes.HasPrefix(rest, []byte(m)) {
			continue
		}
		// The character right after the marker must be a word boundary
		// so "TODOLIST" is not flagged as a TODO.
		boundary := len(m) == len(rest) || isMarkerBoundary(rest[len(m)])
		if !boundary {
			continue
		}
		// Strip a trailing block-comment closer if present.
		msg := strings.TrimRight(string(rest), " \t")
		msg = strings.TrimSuffix(msg, "*/")
		msg = strings.TrimSuffix(msg, "-->")
		msg = strings.TrimSpace(msg)
		return domain.ParseTodo{Line: line, Message: msg}, true
	}
	return domain.ParseTodo{}, false
}

// isMarkerBoundary returns true when b is a character that legitimately
// terminates a TODO marker — colon, whitespace, or a punctuation that
// commonly follows a marker word.
func isMarkerBoundary(b byte) bool {
	switch b {
	case ':', ' ', '\t', '(', '[', '-', '!', '.', '?', ',':
		return true
	}
	return false
}
