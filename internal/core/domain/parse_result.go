// SPDX-License-Identifier: AGPL-3.0-only

package domain

// ParseResult is the output of CodeParser.ParseFile, containing the Nodes and Edges
// extracted from a single source file. Failures contains non-fatal syntax errors
// (such as tree-sitter ERROR or MISSING nodes); a non-empty Failures list does not
// suppress the partially extracted Nodes and Edges.
type ParseResult struct {
	Nodes    []*Node
	Edges    []*Edge
	Failures []ParseFailure
	// Todos contains TODO/FIXME comments detected by the parser's lexical pre-scan.
	// The Ingester collapses this list into a single file-anchored finding per
	// repository, branch, and file.
	Todos []ParseTodo
	// UnresolvedCalls contains call sites whose target callee is not defined in the
	// current file. These are resolved package-wide by the promoter during ingestion.
	UnresolvedCalls []UnresolvedCall
	// Imports maps local package identifiers to full import paths (alias -> path),
	// allowing the promoter to resolve package-qualified unresolved calls.
	Imports map[string]string
}

// UnresolvedCall represents a call site that could not be bound to a target within the
// same file. CallerID is the enclosing node; CalleeName is the resolver lookup key
// (e.g. 'foo' or 'Type.foo'). When PkgQualifier is set, it names the package selector
// (e.g. 'cmd' in 'cmd.Execute') which is resolved using the file imports to create
// either a concrete CALLS edge or a cross-repository edge stub.
type UnresolvedCall struct {
	CallerID     NodeID
	CalleeName   string
	PkgQualifier string
	// IsMethodCall indicates a method call of the form 'v.Method()' where the receiver
	// type is unknown due to the lack of full type inference. The resolver resolves
	// these by looking up the method name inside the imported package, binding if the
	// match is unambiguous.
	IsMethodCall bool
	// SrcLine is the 1-indexed source line of the call expression, allowing resolved
	// edges to be attributed directly to the call site instead of the enclosing node's
	// declaration line. A value of 0 indicates the location is unknown.
	SrcLine int
	// EdgeKind specifies the edge relationship to create when this call resolves,
	// defaulting to EdgeCalls. Framework adaptors can set this to EdgeRoutes to
	// materialize ROUTES edges.
	EdgeKind EdgeKind
}

// ParseFailure describes a syntax-error region surfaced by the parser. Line is 1-based
// and defaults to 0 if the exact location cannot be determined.
type ParseFailure struct {
	Line    int
	Message string
}

// ParseTodo describes a TODO or FIXME comment marker found in the source file,
// capturing the 1-based line number and the trimmed message content.
type ParseTodo struct {
	Line    int
	Message string
}
