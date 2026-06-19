// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// EdgeReaderRepo implements ports.EdgeReader using a SQLite database.
type EdgeReaderRepo struct {
	readDB *sql.DB
}

// NewEdgeReaderRepo constructs an EdgeReaderRepo bound to the given read-only sql.DB connection.
func NewEdgeReaderRepo(readDB *sql.DB) *EdgeReaderRepo {
	return &EdgeReaderRepo{readDB: readDB}
}

// InboundEdges returns a map of destination node IDs to their source node IDs.
func (r *EdgeReaderRepo) InboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error) {
	return r.adjacency(ctx, repoID, branch, nodeIDs, "dst_node_id", "src_node_id", "")
}

// OutboundEdges returns a map of source node IDs to their destination node IDs.
func (r *EdgeReaderRepo) OutboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error) {
	return r.adjacency(ctx, repoID, branch, nodeIDs, "src_node_id", "dst_node_id", "")
}

// InboundCallEdges returns inbound edges filtered by the CALLS kind, matching the behavior of the dead-code check.
func (r *EdgeReaderRepo) InboundCallEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error) {
	return r.adjacency(ctx, repoID, branch, nodeIDs, "dst_node_id", "src_node_id", "CALLS")
}

// adjacency queries edges matching the given direction and optional kind filter.
func (r *EdgeReaderRepo) adjacency(ctx context.Context, repoID, branch string, nodeIDs []string, keyCol, valCol, kind string) (map[string][]string, error) {
	out := make(map[string][]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	for _, id := range nodeIDs {
		out[id] = nil
	}

	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+3)
	args = append(args, repoID, branch)
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	kindClause := ""
	if kind != "" {
		kindClause = " AND UPPER(kind) = ?"
		args = append(args, strings.ToUpper(kind))
	}
	query := fmt.Sprintf(
		`SELECT %s, %s FROM edges
		 WHERE repo_id = ? AND branch = ?
		   AND %s IN (%s)%s`,
		keyCol, valCol, keyCol, strings.Join(placeholders, ","), kindClause,
	)

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("edge_reader: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, val string
		if err := rows.Scan(&key, &val); err != nil {
			return nil, fmt.Errorf("edge_reader: scan: %w", err)
		}
		out[key] = append(out[key], val)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("edge_reader: iterate: %w", err)
	}
	return out, nil
}
