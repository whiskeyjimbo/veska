// Package sqlite contains SQLite-backed adapters for the veska ports layer.
// This file implements ports.NodeLookup against the nodes table — a narrow
// projection used by the application-layer semantic-search service to
// hydrate hits returned by VectorStorage.Search.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// NodeLookupRepo is a SQLite-backed implementation of ports.NodeLookup.
//
// It uses the read-only DB handle: lookups never mutate state and must
// not contend with the single-writer connection.
type NodeLookupRepo struct {
	readDB *sql.DB
}

// NewNodeLookupRepo constructs a NodeLookupRepo backed by readDB.
func NewNodeLookupRepo(readDB *sql.DB) *NodeLookupRepo {
	return &NodeLookupRepo{readDB: readDB}
}

// LookupNodes returns NodeMeta rows for nodeIDs in the given (repoID, branch).
// IDs not present in the nodes table are silently omitted from the result —
// the caller treats the vector index as eventually-consistent and drops any
// hit whose backing row is gone. An empty nodeIDs slice short-circuits to a
// nil result without a database round-trip.
func (r *NodeLookupRepo) LookupNodes(ctx context.Context, repoID, branch string, nodeIDs []string) ([]ports.NodeMeta, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	// Build an IN-list with one placeholder per node_id. The repo_id and
	// branch filters are bound separately and SQLite plans this against
	// idx_nodes_repo_branch + the (node_id, branch) primary key.
	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+2)
	args = append(args, repoID, branch)
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := `SELECT node_id, symbol_path, file_path, kind,
		COALESCE(line_start, 0), COALESCE(line_end, 0),
		COALESCE(snippet, '')
		FROM nodes
		WHERE repo_id = ? AND branch = ?
		  AND node_id IN (` + strings.Join(placeholders, ",") + `)`

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("node_lookup: query: %w", err)
	}
	defer rows.Close()

	out := make([]ports.NodeMeta, 0, len(nodeIDs))
	for rows.Next() {
		var m ports.NodeMeta
		if err := rows.Scan(&m.NodeID, &m.SymbolPath, &m.FilePath, &m.Kind, &m.LineStart, &m.LineEnd, &m.Snippet); err != nil {
			return nil, fmt.Errorf("node_lookup: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("node_lookup: iterate: %w", err)
	}
	return out, nil
}

// NodeContentHash returns nodes.content_hash for nodeID scoped to
// (repoID, branch). An unknown node returns ("", nil) — callers (notably the
// auto-link handler) treat a missing source as "no hash recorded" rather than
// as an error.
//
// This is the per-symbol content_hash on the nodes table; it is intentionally
// distinct from EmbeddingRefRepo.ContentHashForNode, which returns the
// embedding-input hash on node_embedding_refs.
func (r *NodeLookupRepo) NodeContentHash(ctx context.Context, repoID, branch, nodeID string) (string, error) {
	if nodeID == "" {
		return "", nil
	}
	var hash sql.NullString
	err := r.readDB.QueryRowContext(ctx,
		`SELECT content_hash FROM nodes WHERE repo_id = ? AND branch = ? AND node_id = ?`,
		repoID, branch, nodeID,
	).Scan(&hash)
	switch {
	case err == sql.ErrNoRows:
		return "", nil
	case err != nil:
		return "", fmt.Errorf("node_lookup: node content hash: %w", err)
	}
	if !hash.Valid {
		return "", nil
	}
	return hash.String, nil
}

// NodesInFile returns every node_id in (repoID, branch) whose file_path
// equals filePath. The query is served by idx_nodes_repo_branch combined
// with a file_path filter; with the typical "tens of nodes per file" cardinality
// this stays cheap. An unknown path returns (nil, nil).
func (r *NodeLookupRepo) NodesInFile(ctx context.Context, repoID, branch, filePath string) ([]string, error) {
	if filePath == "" {
		return nil, nil
	}
	rows, err := r.readDB.QueryContext(ctx,
		`SELECT node_id FROM nodes WHERE repo_id = ? AND branch = ? AND file_path = ?`,
		repoID, branch, filePath,
	)
	if err != nil {
		return nil, fmt.Errorf("node_lookup: nodes_in_file query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("node_lookup: nodes_in_file scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("node_lookup: nodes_in_file iterate: %w", err)
	}
	return out, nil
}
