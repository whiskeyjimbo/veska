// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// DeadCodeRepo implements ports.DeadCodeQuerier using a SQLite database.
type DeadCodeRepo struct {
	db *sql.DB
}

// NewDeadCodeRepo constructs a DeadCodeRepo bound to the given sql.DB.
func NewDeadCodeRepo(db *sql.DB) *DeadCodeRepo {
	return &DeadCodeRepo{db: db}
}

// DeadNodesInFiles returns nodes in (repoID, branch) whose file_path is one of filePaths and which have zero inbound CALLS edges on that branch.
// An empty slice of filePaths is treated as a no-op.
func (r *DeadCodeRepo) DeadNodesInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ports.NodeRef, error) {
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

	// The query uses a LEFT JOIN to find nodes with zero inbound CALLS edges on the specified branch,
	// filtering out containment and cross-branch edges.
	query := fmt.Sprintf(`
SELECT n.node_id, n.file_path, n.kind, n.symbol_path,
       COALESCE(n.line_start, 0), COALESCE(n.line_end, 0),
       COALESCE(n.content_hash, '')
FROM nodes n
LEFT JOIN edges e
  ON e.dst_node_id = n.node_id AND e.branch = n.branch AND UPPER(e.kind) = 'CALLS'
WHERE n.repo_id = ?
  AND n.branch = ?
  AND n.file_path IN (%s)
  AND e.dst_node_id IS NULL
ORDER BY n.file_path, n.node_id`, strings.Join(placeholders, ","))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite.DeadCodeRepo.DeadNodesInFiles: %w", err)
	}
	defer rows.Close()

	var out []ports.NodeRef
	for rows.Next() {
		var ref ports.NodeRef
		if err := rows.Scan(&ref.NodeID, &ref.FilePath, &ref.Kind, &ref.Name, &ref.LineStart, &ref.LineEnd, &ref.ContentHash); err != nil {
			return nil, fmt.Errorf("sqlite.DeadCodeRepo.DeadNodesInFiles: scan: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.DeadCodeRepo.DeadNodesInFiles: rows: %w", err)
	}
	return out, nil
}

// InterfaceMethodNames returns the distinct method names declared by interface types in (repoID, branch).
func (r *DeadCodeRepo) InterfaceMethodNames(ctx context.Context, repoID, branch string) ([]string, error) {
	const q = `
SELECT DISTINCT substr(m.symbol_path, length(i.symbol_path) + 2)
FROM nodes i
JOIN nodes m
  ON m.repo_id = i.repo_id
 AND m.branch = i.branch
 AND m.kind = 'method'
 AND m.symbol_path LIKE i.symbol_path || '.%'
 AND instr(substr(m.symbol_path, length(i.symbol_path) + 2), '.') = 0
WHERE i.repo_id = ?
  AND i.branch = ?
  AND i.kind = 'interface'`
	rows, err := r.db.QueryContext(ctx, q, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("sqlite.DeadCodeRepo.InterfaceMethodNames: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite.DeadCodeRepo.InterfaceMethodNames: scan: %w", err)
		}
		if name != "" {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.DeadCodeRepo.InterfaceMethodNames: rows: %w", err)
	}
	return out, nil
}
