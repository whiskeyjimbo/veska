// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
// file_path is in filePaths and which have zero incoming edges in the branch.
// InterfaceMethodNames returns the bare method names declared by every
// interface type in (repoID, branch) (e.g., ["String", "Set", "Type"] for
// type Value interface { String() string; Set(string) error }). Concrete
// methods that satisfy a same-repo interface contract are skipped because their
// invocation via interface dispatch is invisible to static graph analysis;
// flagging them as dead would yield false positives.
// The query must not apply any name/kind filtering; those rules live in the
// application layer so they remain testable without a database.
// An empty filePaths slice must return an empty result with no error.
type DeadCodeQuerier interface {
	DeadNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]NodeRef, error)
	InterfaceMethodNames(ctx context.Context, repoID, branch string) ([]string, error)
}
