package domain

// ParseResult is the output of CodeParser.ParseFile.
// It carries the Nodes and Edges extracted from a single source file.
// Both slices may be nil or empty when the file contains no recognisable symbols.
//
// Failures carries any non-fatal syntax errors detected while parsing the file
// (tree-sitter ERROR / MISSING nodes). A non-empty Failures slice does not
// suppress whatever partial Nodes/Edges the parser was able to extract — the
// caller decides what to do with each (typically: stage the partial result and
// raise a 'parse-failure' finding for downstream visibility).
type ParseResult struct {
	Nodes    []*Node
	Edges    []*Edge
	Failures []ParseFailure
	// Todos carries TODO/FIXME-style comments detected by the parser's
	// lexical pre-scan. The list is best-effort: the parser walks the raw
	// source bytes once for the marker and does not attempt to bind the
	// comment to a containing symbol. The Ingester collapses the list into
	// a single file-anchored finding per (repo, branch, file).
	Todos []ParseTodo
}

// ParseFailure describes a single syntax-error region surfaced by the parser.
// Line is 1-based and points to the first ERROR/MISSING node encountered; it
// is best-effort — when the parser cannot pinpoint a location it falls back
// to 0. Message is a short, human-readable reason ("syntax error" by default).
type ParseFailure struct {
	Line    int
	Message string
}

// ParseTodo describes one TODO/FIXME-style marker found in a source file.
// Line is 1-based and points at the line where the marker appeared.
// Message is the rest of the comment line (trimmed) so downstream tools
// can show "TODO: refactor this" without re-reading the file.
type ParseTodo struct {
	Line    int
	Message string
}
