// FindingQuerierRepo backs ports.FindingQuerier by SELECTing open rows
// from the findings table. Reads use the read-only DB handle so the query
// never contends with the single-writer pool used by promotion.

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

// OpenFinding is one open finding projected for the advisory PR report
// enough to tell a reviewer which touched file carries a known
// issue. FilePath coalesces the node's file for node-anchored findings (most
// structural rules set only a node anchor) so file-level intersection works.
type OpenFinding struct {
	FindingID string
	Rule      string
	Severity  string
	FilePath  string
	NodeID    string
	Message   string
}

// OpenFindingsInFiles returns every open finding in (repoID, branch) whose
// (coalesced) file_path is one of filePaths. File-level, so a file-anchored
// finding is not dropped. Empty filePaths is a no-op (nil, nil).
func (r *FindingQuerierRepo) OpenFindingsInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]OpenFinding, error) {
	if len(filePaths) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(filePaths))
	args := make([]any, 0, len(filePaths)+2)
	args = append(args, repoID, branch)
	for i, p := range filePaths {
		placeholders[i] = "?"
		args = append(args, p)
	}
	query := fmt.Sprintf(`
SELECT f.finding_id, f.rule, f.severity,
       COALESCE(f.file_path, n.file_path, '') AS file_path,
       COALESCE(f.node_id, ''),
       f.message
FROM findings f
LEFT JOIN nodes n ON n.node_id = f.node_id AND n.branch = f.branch
WHERE f.repo_id = ? AND f.branch = ? AND f.state = 'open'
  AND COALESCE(f.file_path, n.file_path) IN (%s)
ORDER BY file_path, f.rule, f.finding_id`, strings.Join(placeholders, ","))

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("finding_querier: in-files query: %w", err)
	}
	defer rows.Close()

	var out []OpenFinding
	for rows.Next() {
		var o OpenFinding
		if err := rows.Scan(&o.FindingID, &o.Rule, &o.Severity, &o.FilePath, &o.NodeID, &o.Message); err != nil {
			return nil, fmt.Errorf("finding_querier: in-files scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
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
