package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// NodeLookupRepo is a SQLite-backed implementation of ports.NodeLookup. It uses
// a read-only database handle to avoid contention with the single-writer connection.
type NodeLookupRepo struct {
	readDB *sql.DB
}

// NewNodeLookupRepo constructs a NodeLookupRepo backed by readDB.
func NewNodeLookupRepo(readDB *sql.DB) *NodeLookupRepo {
	return &NodeLookupRepo{readDB: readDB}
}

// LookupNodes retrieves NodeMeta rows for the specified node IDs. Missing IDs
// are silently omitted because semantic search treats the vector index as
// eventually consistent and filters out hits with deleted backing rows.
func (r *NodeLookupRepo) LookupNodes(ctx context.Context, repoID, branch string, nodeIDs []string) ([]ports.NodeMeta, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	// Repository and branch filters are bound separately so SQLite can plan the
	// query using the idx_nodes_repo_branch index alongside the primary key.
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

// NodeContentHash returns the per-symbol content hash for a node. A missing node
// returns an empty string rather than an error because callers (such as the
// auto-link handler) treat it as having no recorded hash. This is distinct
// from the embedding-input hash tracked in node_embedding_refs.
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

// NodesByContentHash retrieves nodes sharing a specific content hash, excluding
// specified kinds. It is used by the exact-clone diff gate to identify potential
// clone groups. The kind filters must align with clone_repo's eligibility rules,
// and empty hashes are rejected early since empty content cannot form clone groups.
func (r *NodeLookupRepo) NodesByContentHash(ctx context.Context, repoID, branch, hash string, excludeKinds []string) ([]ports.NodeRef, error) {
	if hash == "" {
		return nil, nil
	}
	kindClause, kindArgs := notInClause("kind", excludeKinds)
	args := make([]any, 0, 3+len(kindArgs))
	args = append(args, repoID, branch, hash)
	args = append(args, kindArgs...)

	query := `SELECT node_id, file_path, kind, symbol_path,
		COALESCE(line_start, 0), COALESCE(line_end, 0), COALESCE(content_hash, '')
		FROM nodes
		WHERE repo_id = ? AND branch = ? AND content_hash = ?` + kindClause

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("node_lookup: nodes_by_content_hash query: %w", err)
	}
	defer rows.Close()

	out := make([]ports.NodeRef, 0)
	for rows.Next() {
		var ref ports.NodeRef
		if err := rows.Scan(&ref.NodeID, &ref.FilePath, &ref.Kind, &ref.Name,
			&ref.LineStart, &ref.LineEnd, &ref.ContentHash); err != nil {
			return nil, fmt.Errorf("node_lookup: nodes_by_content_hash scan: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("node_lookup: nodes_by_content_hash iterate: %w", err)
	}
	return out, nil
}

// NodesInFile returns all node IDs defined within the specified file.
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
