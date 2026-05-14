package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// DeadCodeRepo is the SQLite adapter for the DeadCodeQuerier port. It answers
// "which nodes in (repoID, branch) whose file_path is in a set have no inbound
// edges on that branch?" in a single round-trip via a LEFT JOIN.
type DeadCodeRepo struct {
	db *sql.DB
}

// NewDeadCodeRepo constructs a DeadCodeRepo bound to the given read-capable
// *sql.DB. The handle must point at a DB with migration 0001 applied (nodes
// + edges tables).
func NewDeadCodeRepo(db *sql.DB) *DeadCodeRepo {
	return &DeadCodeRepo{db: db}
}

// DeadNodesInFiles returns nodes in (repoID, branch) whose file_path is one of
// filePaths and which have ZERO matching rows in edges where dst_node_id =
// node_id AND branch = branch.
//
// Empty filePaths is a no-op (returns nil, nil) — this avoids building a
// degenerate "IN ()" clause that SQLite rejects and is also a cheap fast-path
// for promotions that touched no files of interest.
//
// The query intentionally does not apply name/kind allowlist filtering; that
// rule lives in the application-layer DeadCodeCheck so it is easy to evolve
// and trivial to unit-test without a database.
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

	// LEFT JOIN keeps every candidate node; the WHERE e.dst_node_id IS NULL
	// filter selects rows whose join produced no edge match — i.e. zero inbound
	// edges on this branch. The join condition pins both dst_node_id AND branch
	// so cross-branch edges do not satisfy the join.
	query := fmt.Sprintf(`
SELECT n.node_id, n.file_path, n.kind, n.symbol_path,
       COALESCE(n.line_start, 0), COALESCE(n.line_end, 0)
FROM nodes n
LEFT JOIN edges e
  ON e.dst_node_id = n.node_id AND e.branch = n.branch
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
		if err := rows.Scan(&ref.NodeID, &ref.FilePath, &ref.Kind, &ref.Name, &ref.LineStart, &ref.LineEnd); err != nil {
			return nil, fmt.Errorf("sqlite.DeadCodeRepo.DeadNodesInFiles: scan: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.DeadCodeRepo.DeadNodesInFiles: rows: %w", err)
	}
	return out, nil
}
