package ports

import "context"

// NodeRef is the minimal node metadata needed by the dead-code check to apply
// allowlist filters (kind, name) and to anchor a Finding (node_id, file_path).
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
	// ContentHash is the nodes.content_hash captured at query time. The
	// dead-code check threads it onto the resulting Finding so the
	// revalidation sweep can detect when the symbol's content has drifted
	// and supersede the stale finding.
	ContentHash string
}

// DeadCodeQuerier is the read-side port used by the dead-code structural check.
// DeadNodesInFiles returns the subset of nodes in (repoID, branch) whose
// file_path is in filePaths AND which have zero rows in edges with
// dst_node_id = node_id AND branch = branch.
// InterfaceMethodNames returns the bare method names declared by every
// interface type in (repoID, branch) — e.g. ["String", "Set", "Type"] for
// a repo containing `type Value interface { String string; Set(string)
// error; Type string }`. The dead-code check uses this to skip concrete
// methods that satisfy a same-repo interface contract: a method like
// boolValue.Set is invoked via interface dispatch the static graph cannot
// see, so flagging it as dead is a confident false positive (,
// surfaced by the junior journey on spf13/pflag where ~220 of the
// low-severity findings were Value/SliceValue implementations).
// The query MUST NOT apply any name/kind filtering — those rules live in the
// application layer so they remain trivially testable without a database.
// An empty filePaths slice MUST return an empty result with no error.
type DeadCodeQuerier interface {
	DeadNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]NodeRef, error)
	InterfaceMethodNames(ctx context.Context, repoID, branch string) ([]string, error)
}
