// FindingQuerierRepo backs ports.FindingQuerier by SELECTing open rows
// from the findings table. Reads use the read-only DB handle so the query
// never contends with the single-writer pool used by promotion.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// FindingQuerierRepo is the SQLite-backed adapter for ports.FindingQuerier.
type FindingQuerierRepo struct {
	readDB *sql.DB
}

// NewFindingQuerierRepo constructs a FindingQuerierRepo bound to readDB.
func NewFindingQuerierRepo(readDB *sql.DB) *FindingQuerierRepo {
	return &FindingQuerierRepo{readDB: readDB}
}

// OpenFindingNodeIDs returns the set of node_id values with at least one
// open finding in (repoID, branch). Findings with a NULL node_id are
// skipped by the WHERE clause so they never appear in the result.
func (r *FindingQuerierRepo) OpenFindingNodeIDs(ctx context.Context, repoID, branch string) (map[string]bool, error) {
	const query = `SELECT DISTINCT node_id
	               FROM findings
	               WHERE repo_id = ? AND branch = ? AND state = 'open' AND node_id IS NOT NULL`

	rows, err := r.readDB.QueryContext(ctx, query, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("finding_querier: query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, fmt.Errorf("finding_querier: scan: %w", err)
		}
		out[nodeID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding_querier: iterate: %w", err)
	}
	return out, nil
}

// OpenFindingCountsByRule returns the count of open findings per rule in
// (repoID, branch). Rules with zero open findings are absent from the map.
func (r *FindingQuerierRepo) OpenFindingCountsByRule(ctx context.Context, repoID, branch string) (map[string]int, error) {
	const query = `SELECT rule, COUNT(*)
	               FROM findings
	               WHERE repo_id = ? AND branch = ? AND state = 'open'
	               GROUP BY rule ORDER BY rule`

	rows, err := r.readDB.QueryContext(ctx, query, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("finding_querier: counts query: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var rule string
		var n int
		if err := rows.Scan(&rule, &n); err != nil {
			return nil, fmt.Errorf("finding_querier: counts scan: %w", err)
		}
		out[rule] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding_querier: counts iterate: %w", err)
	}
	return out, nil
}
