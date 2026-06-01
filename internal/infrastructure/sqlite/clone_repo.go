// This file implements application/duplicates.CloneStore against the nodes
// table: the exact-clone projection that groups nodes by shared content_hash.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
)

// CloneRepo is a SQLite-backed implementation of duplicates.CloneStore.
//
// It uses the read-only DB handle: the query never mutates state and must not
// contend with the single-writer connection.
type CloneRepo struct {
	readDB *sql.DB
}

// NewCloneRepo constructs a CloneRepo backed by readDB.
func NewCloneRepo(readDB *sql.DB) *CloneRepo {
	return &CloneRepo{readDB: readDB}
}

// ClonedNodes returns every node in (repoID, branch) whose content_hash is
// shared by >=2 nodes, excluding excludeKinds. The set is selected by a
// GROUP BY ... HAVING COUNT(*) >= 2 subquery over content_hash; the outer
// query re-joins to hydrate each member's metadata. Both the subquery and the
// outer query apply the same repo/branch + kind filter so the COUNT reflects
// only eligible nodes (a chunk sharing a hash with a function must not inflate
// the count). idx_nodes_content_hash + idx_nodes_repo_branch serve it.
//
// content_hash is NOT NULL on the schema, so no NULL guard is needed.
func (r *CloneRepo) ClonedNodes(ctx context.Context, repoID, branch string, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	// Two copies of (repoID, branch) — one per query level — then the kind
	// list once per level. Built positionally to keep SQLite's planner on the
	// indexed path rather than a named-param rewrite.
	kindClause, kindArgs := notInClause("kind", excludeKinds)

	args := make([]any, 0, 4+2*len(excludeKinds))
	args = append(args, repoID, branch)
	args = append(args, kindArgs...)
	args = append(args, repoID, branch)
	args = append(args, kindArgs...)

	query := `SELECT content_hash, node_id, symbol_path, file_path, kind,
		COALESCE(line_start, 0), COALESCE(line_end, 0)
		FROM nodes
		WHERE repo_id = ? AND branch = ?` + kindClause + `
		  AND content_hash IN (
			SELECT content_hash FROM nodes
			WHERE repo_id = ? AND branch = ?` + kindClause + `
			GROUP BY content_hash HAVING COUNT(*) >= 2
		  )`

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clone_repo: query: %w", err)
	}
	defer rows.Close()

	out := make([]duplicates.ClonedNode, 0)
	for rows.Next() {
		var n duplicates.ClonedNode
		if err := rows.Scan(&n.ContentHash, &n.NodeID, &n.SymbolPath, &n.FilePath, &n.Kind, &n.LineStart, &n.LineEnd); err != nil {
			return nil, fmt.Errorf("clone_repo: scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clone_repo: iterate: %w", err)
	}
	return out, nil
}

// notInClause builds a " AND <col> NOT IN (?, ?, …)" fragment plus its bound
// args. An empty values slice yields an empty clause and no args, so the caller
// can concatenate unconditionally.
func notInClause(col string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return " AND " + col + " NOT IN (" + strings.Join(placeholders, ",") + ")", args
}

// Compile-time check that *CloneRepo satisfies the consumer-owned port.
var _ duplicates.CloneStore = (*CloneRepo)(nil)
