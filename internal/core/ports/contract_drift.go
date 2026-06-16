package ports

import "context"

// DriftedNode is the minimal projection needed by the contract-drift check to
// build a Finding. It carries both halves of the signature comparison so the
// finding message can include a useful before/after snippet.
// The kind filter (function/method/interface) is applied in the adapter SQL
// because it is a closed enum the storage layer can evaluate cheaply; all
// other policy (severity, message shape, anchor selection) lives in the
// application layer.
type DriftedNode struct {
	NodeID    string
	FilePath  string
	Kind      string
	Name      string
	PrevSig   string
	NewSig    string
	LineStart int
	LineEnd   int
	// ContentHash is the CURRENT nodes.content_hash (after the drift). The
	// contract-drift check threads it onto the resulting Finding so the
	// revalidation sweep can supersede the finding once content drifts again.
	ContentHash string
	// Exported is the node's visibility flag (nodes.exported; NULL coalesced to
	// false). The whole-repo contract-drift check ignores it — it flags drift of
	// any visibility — but the breaking-exported-signature diff gate
	// filters on it so only public-surface drift fails CI.
	Exported bool
}

// ContractDriftQuerier is the read-side port used by the contract-drift
// structural check.
// DriftedNodesInFiles returns the subset of nodes in (repoID, branch) whose
// file_path is in filePaths AND whose prev_signature differs from signature
// AND whose kind is one of {function, method, interface}.
// An empty filePaths slice MUST return an empty result with no error — this
// keeps the contract symmetric with DeadCodeQuerier and avoids degenerate
// "IN " clauses at the adapter layer.
type ContractDriftQuerier interface {
	DriftedNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]DriftedNode, error)
}
