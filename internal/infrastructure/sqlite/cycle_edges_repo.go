package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DependencyEdge is a directed dependency edge with both endpoints resolved to
// their file and symbol — the projection the cycle diff-gate
// needs to build the after-/base-state graphs (partition by endpoint file to
// splice the re-promote clone) and to name cycle members without a second
// round-trip.
type DependencyEdge struct {
	SrcID, DstID         string
	SrcFile, DstFile     string
	SrcSymbol, DstSymbol string
}

// CycleEdgeRepo answers "the directed dependency edges (of the given kinds) in
// (repoID, branch)" for the cycle gate. It joins each edge to its source and
// destination nodes so the caller gets endpoint files and symbols inline.
type CycleEdgeRepo struct{ db *sql.DB }

// NewCycleEdgeRepo constructs a CycleEdgeRepo bound to a read-capable *sql.DB
// whose schema has migration 0001 applied (nodes + edges tables).
func NewCycleEdgeRepo(db *sql.DB) *CycleEdgeRepo { return &CycleEdgeRepo{db: db} }

// DependencyEdges returns every edge in (repoID, branch) whose kind is in kinds
// (case-insensitive), with both endpoints resolved to (file_path, symbol_path).
// An edge whose src or dst node is absent is dropped (INNER JOINs). Empty kinds
// is a no-op (nil, nil), avoiding a degenerate "IN " clause.
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
