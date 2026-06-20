// SPDX-License-Identifier: AGPL-3.0-only

package treesitter

import (
	"bytes"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// todoMarkers lists the target keywords scanned by scanTodos. Generic terms like
// NOTE or HACK are omitted to maintain high signal quality.
var todoMarkers = []string{"TODO", "FIXME", "XXX"}

// scanTodos extracts TODO, FIXME, and XXX comments from source text. The scanner is
// lexical and language-agnostic to avoid the overhead of language-specific parsers.
// Because the scanner is lexical, it may flag markers inside string literals. Returned
// line numbers are 1-based. The message starts from the marker keyword and extends
// to the end of the line with comment closers (like `*/` or `-->`) stripped.
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

// matchTodoLine parses a single line and returns a ParseTodo if it contains a
// supported marker keyword.
func matchTodoLine(raw []byte, line int) (domain.ParseTodo, bool) {
	// Skip preceding whitespace and comment leaders.
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
		// Check that the marker is followed by a word boundary to prevent matching prefixes
		// (such as 'TODOLIST').
		boundary := len(m) == len(rest) || isMarkerBoundary(rest[len(m)])
		if !boundary {
			continue
		}
		// Strip trailing block comment closers.
		msg := strings.TrimRight(string(rest), " \t")
		msg = strings.TrimSuffix(msg, "*/")
		msg = strings.TrimSuffix(msg, "-->")
		msg = strings.TrimSpace(msg)
		return domain.ParseTodo{Line: line, Message: msg}, true
	}
	return domain.ParseTodo{}, false
}

// isMarkerBoundary returns true if the given byte is a valid separator following
// a marker keyword.
func isMarkerBoundary(b byte) bool {
	switch b {
	case ':', ' ', '\t', '(', '[', '-', '!', '.', '?', ',':
		return true
	}
	return false
}
