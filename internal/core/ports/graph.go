package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GraphStorage is the write-side port for the code graph. It mirrors
// EdgeStorage: a narrow set of mutating operations, kept separate from the
// read surface (GraphReader) so read-only consumers — the MCP graph tools,
// blast-radius, call-chain — depend only on what they use. Implementations
// are provided by infrastructure adapters (e.g. SQLite GraphRepo).
//
// Production graph writes flow through application.PromotionStore inside the
// promotion transaction; these methods exist for the adapter's own
// round-trip coverage and any future non-promotion writer.
type GraphStorage interface {
	// SaveNode inserts or replaces a Node for the given repository and branch.
	// The node's ID is used as the upsert key.
	SaveNode(ctx context.Context, repoID, branch string, n *domain.Node) error

	// SaveEdge inserts or replaces an Edge for the given repository and branch.
	// Edges are keyed on (From, To, Kind).
	SaveEdge(ctx context.Context, repoID, branch string, e *domain.Edge) error

	// DeleteFile removes all Nodes and Edges whose source file matches filePath
	// for the given repository and branch.
	DeleteFile(ctx context.Context, repoID, branch, filePath string) error
}

// GraphReader is the read-side companion to GraphStorage, mirroring the
// EdgeReader/EdgeStorage split. It is the port the MCP graph tools and the
// graph-walking services (blast-radius, call-chain) depend on; none of them
// mutate the graph, so they take this narrow interface rather than the full
// storage port. Implementations are provided by infrastructure adapters
// (e.g. SQLite GraphRepo).
type GraphReader interface {
	// LoadGraph builds and returns the full in-memory Graph for the given
	// repository and branch. Returns a non-nil empty Graph when no data is stored.
	LoadGraph(ctx context.Context, repoID, branch string) (*domain.Graph, error)

	// FindNodes returns all Nodes whose symbol name equals symbolName (exact match)
	// in the given repository and branch.
	FindNodes(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error)

	// GetNode retrieves a single Node by its NodeID. Returns nil, nil when not found.
	GetNode(ctx context.Context, repoID, branch string, id domain.NodeID) (*domain.Node, error)

	// FindNodeByID looks up a Node by its content-hashed NodeID, scanning
	// across every (repo_id, branch) pair. Used by eng_get_node so the caller
	// can omit repo_id+branch when they already have the (globally unique)
	// node_id . Returns nil, nil when not found.
	FindNodeByID(ctx context.Context, id domain.NodeID) (*domain.Node, error)

	// FindNodeIDsByPrefix returns the distinct node_ids that begin with prefix,
	// scanning across every (repo_id, branch) pair, capped at limit. It exists
	// so eng_get_node can resolve the 12-char display prefix that
	// eng_find_symbol / `veska symbol` print, not just the full 64-char id
	// (solov2-uej9.3). Implementations DISTINCT on node_id so a node present on
	// multiple branches is not mistaken for an ambiguous prefix. The caller
	// (eng_get_node) treats len>1 as an ambiguous-prefix error listing the
	// candidates and len==1 as the resolved id. Returns an empty slice (not an
	// error) when nothing matches.
	FindNodeIDsByPrefix(ctx context.Context, prefix string, limit int) ([]domain.NodeID, error)

	// NodesForFile returns every Node whose file_path equals filePath in the
	// given repository and branch. Returns an empty slice (not an error) when
	// the file has no promoted nodes. This is the primary read for
	// eng_get_file_nodes; promoting it to the port retires the optional
	// type-assertion dance the handler used to do .
	NodesForFile(ctx context.Context, repoID, branch, filePath string) ([]*domain.Node, error)

	// GetNodeSnippet returns the persisted capped body for a single node.
	// Returns "" (not an error) when the row exists but stored NULL, and
	// "" with sql.ErrNoRows-equivalent treatment when the row is missing.
	// Implementations cap the returned bytes (sqlite uses maxSnippetBytes)
	// so callers must not assume the snippet equals the full source.
	// Used by eng_get_call_chain to discriminate the
	// chained_selectors_unresolved / external_callees_only degraded reasons
	// (solov2-izh6.22).
	GetNodeSnippet(ctx context.Context, repoID, branch string, id domain.NodeID) (string, error)
}
