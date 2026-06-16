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
// filePaths and which have ZERO inbound CALLS edges on that branch.
// Liveness is measured against CALLS edges ONLY — not every edge kind. Every
// symbol has an inbound CONTAINS edge from its package and may have SIMILAR_TO
// edges from autolink; counting those made the check inert (nothing was ever
// "dead"), which is. "No inbound CALLS" is the meaningful
// uncalled-symbol signal. Exported symbols (callable from other packages /
// repos, where call edges may be invisible) are excluded upstream by the
// application-layer DeadCodeCheck allowlist, so this does not mis-flag them.
// Empty filePaths is a no-op (returns nil, nil) — this avoids building a
// degenerate "IN " clause that SQLite rejects and is also a cheap fast-path
// for promotions that touched no files of interest.
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
	// CALLS edges on this branch. The join pins dst_node_id, branch AND the
	// CALLS kind (case-insensitive) so containment/similarity edges and
	// cross-branch edges never count as liveness.
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

// InterfaceMethodNames returns the distinct bare method names declared by
// every interface type in (repoID, branch). An interface method node has
// kind='method' and symbol_path '<IfaceName>.<MethodName>'; the parent
// type's node has kind='interface'. The query joins the two so an
// orphan method node (e.g. created by a malformed parse) does not bleed
// into the result. Result strings are bare method names ('Set', 'String')
// the dead-code application filter compares against a method's bare
// suffix.
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
