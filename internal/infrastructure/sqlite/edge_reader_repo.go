// EdgeReaderRepo backs ports.EdgeReader against the edges table. Reads
// take the read-only connection so they never contend with the
// single-writer pool used by promotion.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// EdgeReaderRepo is the SQLite-backed adapter for ports.EdgeReader.
type EdgeReaderRepo struct {
	readDB *sql.DB
}

// NewEdgeReaderRepo constructs an EdgeReaderRepo bound to readDB. Pass
// the read-only handle so adjacency walks do not contend with promotion.
func NewEdgeReaderRepo(readDB *sql.DB) *EdgeReaderRepo {
	return &EdgeReaderRepo{readDB: readDB}
}

// InboundEdges returns dst_node_id → [src_node_id, ...] for each node_id
// that appears as a destination in the edges table.
func (r *EdgeReaderRepo) InboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error) {
	return r.adjacency(ctx, repoID, branch, nodeIDs, "dst_node_id", "src_node_id")
}

// OutboundEdges returns src_node_id → [dst_node_id, ...] for each node_id
// that appears as a source in the edges table.
func (r *EdgeReaderRepo) OutboundEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error) {
	return r.adjacency(ctx, repoID, branch, nodeIDs, "src_node_id", "dst_node_id")
}

// adjacency is the shared body of InboundEdges and OutboundEdges. keyCol
// is the column we filter on (the "from" side of the lookup) and valCol
// is the column we return (the "to" side).
func (r *EdgeReaderRepo) adjacency(ctx context.Context, repoID, branch string, nodeIDs []string, keyCol, valCol string) (map[string][]string, error) {
	out := make(map[string][]string, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	// Seed the map so callers can rely on "queried = present".
	for _, id := range nodeIDs {
		out[id] = nil
	}

	placeholders := make([]string, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+2)
	args = append(args, repoID, branch)
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := fmt.Sprintf(
		`SELECT %s, %s FROM edges
		 WHERE repo_id = ? AND branch = ?
		   AND %s IN (%s)`,
		keyCol, valCol, keyCol, strings.Join(placeholders, ","),
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
