// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// NodeMeta is the projection of a node row needed to hydrate a vector-search hit.
// It is narrower than domain.Node because the search path only requires identification
// and source location, not raw content or content hashes.
type NodeMeta struct {
	NodeID     string
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// Snippet is the symbol's stored content. Search hydrates this through to
	// Result.Snippet so callers get the bytes inline and can skip a separate read call.
	Snippet string
}

// NodeLookup hydrates node IDs into metadata. It is used by the search service
// to project vector search hits into renderable results. Implementations must
// scope the lookup to (repoID, branch) because node IDs are only unique within
// a branch. IDs not present in storage are silently omitted from the result;
// callers treat the index as eventually consistent with SQL and drop dangling hits.
type NodeLookup interface {
	LookupNodes(ctx context.Context, repoID, branch string, nodeIDs []string) ([]NodeMeta, error)

	// NodesInFile returns every node ID in the branch whose file path equals
	// filePath. It is used to translate a per-file promotion payload into the
	// source nodes fed to the Linker. An unknown file path returns nil with
	// no error, which is treated as a no-op.
	NodesInFile(ctx context.Context, repoID, branch, filePath string) ([]string, error)
}
