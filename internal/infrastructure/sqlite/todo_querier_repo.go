// TodoQuerierRepo backs ports.TodoQuerier by SELECTing rule='todo' rows
// from the findings table. Reads use the read-only DB handle so the
// query never contends with the single-writer pool used by promotion.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TodoQuerierRepo is the SQLite-backed adapter for ports.TodoQuerier.
type TodoQuerierRepo struct {
	readDB *sql.DB
}

// NewTodoQuerierRepo constructs a TodoQuerierRepo bound to readDB.
func NewTodoQuerierRepo(readDB *sql.DB) *TodoQuerierRepo {
	return &TodoQuerierRepo{readDB: readDB}
}

// FindTodos returns every 'todo'-rule finding scoped to (repoID, branch).
// When onlyOpen is true the WHERE clause restricts to state='open'.
func (r *TodoQuerierRepo) FindTodos(ctx context.Context, repoID, branch string, onlyOpen bool) ([]ports.TodoEntry, error) {
	query := `SELECT finding_id, repo_id, branch, COALESCE(file_path, ''), message, state, created_at
	          FROM findings
	          WHERE rule = 'todo' AND repo_id = ? AND branch = ?`
	args := []any{repoID, branch}
	if onlyOpen {
		query += ` AND state = 'open'`
	}
	query += ` ORDER BY file_path, created_at`

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("todo_querier: query: %w", err)
	}
	defer rows.Close()

	var out []ports.TodoEntry
	for rows.Next() {
		var e ports.TodoEntry
		if err := rows.Scan(&e.FindingID, &e.RepoID, &e.Branch, &e.FilePath, &e.Message, &e.State, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("todo_querier: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("todo_querier: iterate: %w", err)
	}
	return out, nil
}
