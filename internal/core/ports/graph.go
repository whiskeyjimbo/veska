package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GraphStorage is the port for persisting and querying the code graph.
// Implementations are provided by infrastructure adapters (e.g. Dolt, SQLite).
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
	// node_id (solov2-v4ob). Returns nil, nil when not found.
	FindNodeByID(ctx context.Context, id domain.NodeID) (*domain.Node, error)

	// NodesForFile returns every Node whose file_path equals filePath in the
	// given repository and branch. Returns an empty slice (not an error) when
	// the file has no promoted nodes. This is the primary read for
	// eng_get_file_nodes; promoting it to the port retires the optional
	// type-assertion dance the handler used to do (solov2-8ex).
	NodesForFile(ctx context.Context, repoID, branch, filePath string) ([]*domain.Node, error)
}
