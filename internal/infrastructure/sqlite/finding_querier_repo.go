package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// FindingQuerierRepo implements ports.FindingQuerier by querying open rows from
// the findings table. It uses the read-only DB handle to prevent query contention
// with the single-writer pool.
type FindingQuerierRepo struct {
	readDB *sql.DB
}

// NewFindingQuerierRepo constructs a FindingQuerierRepo.
func NewFindingQuerierRepo(readDB *sql.DB) *FindingQuerierRepo {
	return &FindingQuerierRepo{readDB: readDB}
}

// OpenFindingNodeIDs returns a map of node IDs that have at least one open
// finding. Findings without a node ID are excluded.
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

// OpenFinding represents an open finding projected for the advisory PR report.
// FilePath coalesces the associated node's file path for node-anchored findings
// so file-level intersection logic works correctly.
type OpenFinding struct {
	FindingID string
	Rule      string
	Severity  string
	FilePath  string
	NodeID    string
	Message   string
}

// OpenFindingsInFiles returns all open findings that match the given file
// paths. An empty slice of file paths returns early.
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

// OpenFindingCountsByRule returns counts of open findings grouped by rule.
// Rules with no open findings are excluded.
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

