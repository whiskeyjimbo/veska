// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// GraphStorage is the write-side port for the code graph, separated from the
// read surface (GraphReader) so read-only consumers depend only on what they use.
// These methods exist for round-trip test coverage and future non-promotion write paths.
type GraphStorage interface {
	SaveNode(ctx context.Context, repoID, branch string, n *domain.Node) error

	SaveEdge(ctx context.Context, repoID, branch string, e *domain.Edge) error

	DeleteFile(ctx context.Context, repoID, branch, filePath string) error
}

// GraphReader is the read-side companion to GraphStorage, mirroring the
// EdgeReader/EdgeStorage split. Read-only services depend on this narrow
// interface rather than the full storage port.
type GraphReader interface {
	LoadGraph(ctx context.Context, repoID, branch string) (*domain.Graph, error)

	FindNodes(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error)

	GetNode(ctx context.Context, repoID, branch string, id domain.NodeID) (*domain.Node, error)

	// FindNodeByID looks up a Node by its content-hashed NodeID across all
	// repositories and branches. This allows callers to omit repoID and branch
	// when they have the globally unique node ID.
	FindNodeByID(ctx context.Context, id domain.NodeID) (*domain.Node, error)

	// FindNodeIDsByPrefix returns distinct node IDs that begin with prefix,
	// capped at limit. This resolves the 12-character display prefix printed
	// by symbol tools. Implementations select distinct node IDs so a node
	// present on multiple branches is not mistaken for an ambiguous prefix.
	// A length greater than one represents an ambiguous prefix.
	FindNodeIDsByPrefix(ctx context.Context, prefix string, limit int) ([]domain.NodeID, error)

	// NodesForFile returns every Node in the given repository and branch whose
	// file path equals filePath. It returns an empty slice if the file has no
	// promoted nodes.
	NodesForFile(ctx context.Context, repoID, branch, filePath string) ([]*domain.Node, error)

	// GetNodeSnippet returns the persisted snippet for a single node. The
	// returned content may be capped, so callers must not assume it equals
	// the full source.
	GetNodeSnippet(ctx context.Context, repoID, branch string, id domain.NodeID) (string, error)
}
