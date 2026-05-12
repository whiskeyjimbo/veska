package ports

import (
	"context"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
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
}
