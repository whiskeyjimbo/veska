package ports

import "context"

// NodeMeta is the minimal projection of a node row needed to hydrate a
// vector-search hit. It is intentionally narrower than domain.Node — the
// search path only needs identification and source location, not raw
// content or content hashes.
type NodeMeta struct {
	NodeID     string
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// Snippet is the symbol's stored raw_content (nodes.snippet column).
	// Populated by adapters that select the column; empty when the
	// caller is on a code path that did not request it. Search hydrates
	// this through to Result.Snippet so agents get the bytes inline and
	// can skip a separate Read call (solov2-7kz).
	Snippet string
}

// NodeLookup is the port for hydrating a set of node IDs into their
// minimal metadata. It is used by the application-layer search service
// to project VectorStorage.Search hits (which carry only node_id and
// score) into results that callers can render.
//
// Implementations must scope the lookup to (repoID, branch); a node_id
// is only unique within a branch. IDs not present in storage are
// silently omitted from the result — callers treat the index as
// eventually-consistent vs the SQL truth and drop dangling hits.
type NodeLookup interface {
	LookupNodes(ctx context.Context, repoID, branch string, nodeIDs []string) ([]NodeMeta, error)

	// NodesInFile returns every node_id in (repoID, branch) whose file_path
	// equals filePath. Used by the auto-link queue handler to translate the
	// per-file promotion payload into the set of source nodes fed to the
	// Linker. An unknown file path returns (nil, nil): the queue handler
	// treats it as a no-op rather than an error.
	NodesInFile(ctx context.Context, repoID, branch, filePath string) ([]string, error)
}
