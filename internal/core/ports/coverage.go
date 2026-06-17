package ports

import "context"

// NodeCallers pairs a candidate node with the distinct file paths of its
// direct inbound callers. It is the read-side attribution that the
// untested-symbol check consumes. A node whose CallerFiles contains no
// test-shaped path is "untested" under the CALLS-edge proxy.
// The slice holds caller file paths, not full node references. The test-file
// classification is applied in the application layer, keeping language-specific
// naming rules out of the adapter SQL. A node with no inbound calls appears
// with an empty CallerFiles slice. Attribution is direct only (one CALLS hop
// from a caller node).
type NodeCallers struct {
	Node        NodeRef
	CallerFiles []string
}

// CoverageQuerier is the read-side port behind the untested-symbol structural
// check. It reuses NodeRef so the check can anchor a Finding without pulling
// the full Node aggregate into the query path.
// CandidateCallersInFiles returns every node in (repoID, branch) whose
// file_path is one of filePaths, each paired with the distinct file paths of
// its direct inbound CALLS callers. The query must not apply any name/kind
// allowlist filtering or test-file classification; those rules live in the
// application-layer check so they stay testable without a database.
// An empty filePaths slice must return an empty result with no error.
type CoverageQuerier interface {
	CandidateCallersInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]NodeCallers, error)
}
