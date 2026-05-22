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
	// UnresolvedCalls carries call sites whose callee is named by the
	// source but is not in the file's symbol map — typically because
	// the callee lives in another file of the same Go package. The
	// promoter resolves these against a per-package map built from the
	// whole batch and emits CALLS edges in the same transaction
	// (solov2-2at).
	UnresolvedCalls []UnresolvedCall
	// Imports maps a file's local package identifiers to their full import
	// paths (alias -> path; for unaliased imports the key is the path's last
	// segment, matching the common case where the package name equals it).
	// Promotion uses this to resolve package-qualified UnresolvedCalls
	// (solov2-xc51). nil/empty when the file imports nothing.
	Imports map[string]string
}

// UnresolvedCall is one call site the parser saw but could not bind to
// a target within the same file. CallerID is the in-file node that
// contains the call; CalleeName is the lookup key for the package-wide
// resolver — either "foo" for a plain-identifier call or "Type.foo" for
// a receiver-method call (the receiver type having been determined from
// the enclosing method_declaration).
//
// PkgQualifier, when non-empty, names the selector operand of a
// package-qualified call (the "cmd" in cmd.Execute()). At promotion time it
// is resolved against the file's import map ([[ParseResult]].Imports) to a
// package — intra-module packages bind to a concrete CALLS edge, external
// modules become a cross-repo edge stub (solov2-xc51). When PkgQualifier is
// empty the call is plain/receiver-local as before.
type UnresolvedCall struct {
	CallerID     NodeID
	CalleeName   string
	PkgQualifier string
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
