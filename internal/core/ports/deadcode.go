package ports

import "context"

// NodeRef is the minimal node metadata needed by the dead-code check to apply
// allowlist filters (kind, name) and to anchor a Finding (node_id, file_path).
//
// It is intentionally a value type with no domain dependency: the dead-code
// check does not need the full Node aggregate, and keeping this thin avoids
// pulling the application layer into the adapter's query path.
type NodeRef struct {
	NodeID    string
	FilePath  string
	Kind      string
	Name      string
	LineStart int
	LineEnd   int
}

// DeadCodeQuerier is the read-side port used by the dead-code structural check.
//
// DeadNodesInFiles returns the subset of nodes in (repoID, branch) whose
// file_path is in filePaths AND which have zero rows in edges with
// dst_node_id = node_id AND branch = branch.
//
// The query MUST NOT apply any name/kind filtering — those rules live in the
// application layer so they remain trivially testable without a database.
// An empty filePaths slice MUST return an empty result with no error.
type DeadCodeQuerier interface {
	DeadNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]NodeRef, error)
}
