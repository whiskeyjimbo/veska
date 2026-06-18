// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DependencyEdge represents a directed dependency edge where both endpoints are resolved to their respective file paths and symbol names.
type DependencyEdge struct {
	SrcID, DstID         string
	SrcFile, DstFile     string
	SrcSymbol, DstSymbol string
}

// CycleEdgeRepo retrieves directed dependency edges joined with endpoint files and symbols.
type CycleEdgeRepo struct{ db *sql.DB }

// NewCycleEdgeRepo constructs a CycleEdgeRepo bound to a sql.DB.
func NewCycleEdgeRepo(db *sql.DB) *CycleEdgeRepo { return &CycleEdgeRepo{db: db} }

// DependencyEdges returns directed dependency edges in (repoID, branch) matching the specified kinds.
// An empty slice of kinds is treated as a no-op.
func (r *CycleEdgeRepo) DependencyEdges(ctx context.Context, repoID, branch string, kinds []string) ([]DependencyEdge, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(kinds))
	args := make([]any, 0, len(kinds)+2)
	args = append(args, repoID, branch)
	for i, k := range kinds {
		placeholders[i] = "?"
		args = append(args, strings.ToUpper(k))
	}

	query := fmt.Sprintf(`
SELECT e.src_node_id, e.dst_node_id,
       sn.file_path, sn.symbol_path,
       dn.file_path, dn.symbol_path
FROM edges e
JOIN nodes sn ON sn.node_id = e.src_node_id AND sn.branch = e.branch
JOIN nodes dn ON dn.node_id = e.dst_node_id AND dn.branch = e.branch
WHERE e.repo_id = ? AND e.branch = ? AND UPPER(e.kind) IN (%s)`,
		strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.CycleEdgeRepo.DependencyEdges: %w", err)
	}
	defer rows.Close()

	var out []DependencyEdge
	for rows.Next() {
		var e DependencyEdge
		if err := rows.Scan(&e.SrcID, &e.DstID, &e.SrcFile, &e.SrcSymbol, &e.DstFile, &e.DstSymbol); err != nil {
			return nil, fmt.Errorf("sqlite.CycleEdgeRepo.DependencyEdges: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
