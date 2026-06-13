package ports

import "context"

// NodeCallers pairs a candidate node with the distinct file paths of its
// DIRECT inbound CALLS callers. It is the read-side attribution that the
// untested-symbol check (solov2-zvh6.3) consumes: a node whose CallerFiles
// contains no test-shaped path is "untested" under the CALLS-edge proxy.
//
// The slice holds caller FILE PATHS, not full node refs, and the test-file
// classification is applied in the application layer (the existing isTestFile
// vocabulary) — keeping the language-specific naming rules out of the adapter
// SQL, consistent with the dead-code check. A node with no inbound CALLS
// caller appears with an empty (non-nil-irrelevant) CallerFiles slice.
//
// Attribution is DIRECT only — one CALLS hop from a caller node. The
// transitive node→test reverse map is a deliberate extension tracked
// separately (solov2-v6de.1); this port stays presence-grade.
type NodeCallers struct {
	Node        NodeRef
	CallerFiles []string
}

// CoverageQuerier is the read-side port behind the untested-symbol structural
// check. It reuses NodeRef (declared for the dead-code querier) so the check
// can anchor a Finding without pulling the full Node aggregate into the query
// path.
//
// CandidateCallersInFiles returns every node in (repoID, branch) whose
// file_path is one of filePaths, each paired with the distinct file paths of
// its direct inbound CALLS callers. The query MUST NOT apply any name/kind
// allowlist filtering or test-file classification — those rules live in the
// application-layer check so they stay trivially testable without a database.
// An empty filePaths slice MUST return an empty result with no error.
type CoverageQuerier interface {
	CandidateCallersInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]NodeCallers, error)
}
