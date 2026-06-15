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
// Empty content_hash is excluded from grouping: content_hash is NOT NULL on the
// schema, but nodes with no raw content (and, before solov2-ozoi.2, every parsed
// node) carry the empty string. Grouping by an empty hash would bucket all of
// them into one bogus byte-identical clone group, so both query levels filter
// empty out with a non-empty content_hash check —
// "no content known" can never be a clone match.
func (r *CloneRepo) ClonedNodes(ctx context.Context, q duplicates.CloneQuery, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	// scope() emits the shared per-level predicate (branch, optional repo +
	// path) plus the kind exclusion, so the outer query and the GROUP BY
	// subquery filter identically — a chunk in one repo must not inflate a
	// content_hash COUNT in another. Built positionally to keep SQLite's planner
	// on the indexed path rather than a named-param rewrite.
	scope, scopeArgs := cloneScopeClause(q, excludeKinds)

	args := make([]any, 0, 2*len(scopeArgs))
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)

	query := `SELECT content_hash, repo_id, node_id, symbol_path, file_path, kind,
		COALESCE(line_start, 0), COALESCE(line_end, 0)
		FROM nodes
		WHERE ` + scope + `
		  AND content_hash != ''
		  AND content_hash IN (
			SELECT content_hash FROM nodes
			WHERE ` + scope + `
			  AND content_hash != ''
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
		if err := rows.Scan(&n.ContentHash, &n.RepoID, &n.NodeID, &n.SymbolPath, &n.FilePath, &n.Kind, &n.LineStart, &n.LineEnd); err != nil {
			return nil, fmt.Errorf("clone_repo: scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clone_repo: iterate: %w", err)
	}
	return out, nil
}

// StructuralNodes is the Type-2 (renamed-variable) analogue of ClonedNodes: it
// groups by structural_hash instead of content_hash, so nodes with the same
// shape after a consistent rename cluster even when their verbatim text differs.
// structural_hash is NULLABLE (the parser sets it only for structurally-hashed
// kinds), so the empty-string guard ClonedNodes uses becomes an IS NOT NULL
// guard — NULL "no structural signal" can never be a clone match. The grouping
// hash is returned in ClonedNode.ContentHash so the Finder folds it identically.
// idx_nodes_structural_hash + idx_nodes_repo_branch serve it.
func (r *CloneRepo) StructuralNodes(ctx context.Context, q duplicates.CloneQuery, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	scope, scopeArgs := cloneScopeClause(q, excludeKinds)

	args := make([]any, 0, 2*len(scopeArgs))
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)

	query := `SELECT structural_hash, content_hash, repo_id, node_id, symbol_path, file_path, kind,
		COALESCE(line_start, 0), COALESCE(line_end, 0)
		FROM nodes
		WHERE ` + scope + `
		  AND structural_hash IS NOT NULL
		  AND structural_hash IN (
			SELECT structural_hash FROM nodes
			WHERE ` + scope + `
			  AND structural_hash IS NOT NULL
			GROUP BY structural_hash HAVING COUNT(*) >= 2
		  )`

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clone_repo: structural query: %w", err)
	}
	defer rows.Close()

	out := make([]duplicates.ClonedNode, 0)
	for rows.Next() {
		var n duplicates.ClonedNode
		if err := rows.Scan(&n.StructuralHash, &n.ContentHash, &n.RepoID, &n.NodeID, &n.SymbolPath, &n.FilePath, &n.Kind, &n.LineStart, &n.LineEnd); err != nil {
			return nil, fmt.Errorf("clone_repo: structural scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clone_repo: structural iterate: %w", err)
	}
	return out, nil
}

// SimilarEdges returns persisted SIMILAR_TO edges in (repoID, branch) whose
// score is non-NULL and >= minScore and whose BOTH endpoints fall outside
// excludeKinds, with endpoint metadata hydrated by joining the nodes table on
// (node_id, branch) — the (node_id, branch) primary key serves each join.
// idx_edges_repo_branch selects the edge set; the score predicate prunes the
// SIMILAR_TO subset further. NULL-score rows (legacy edges promoted before the
// score column existed) are excluded by the IS NOT NULL guard.
func (r *CloneRepo) SimilarEdges(ctx context.Context, repoID, branch string, minScore float32, excludeKinds []string) ([]duplicates.SimilarEdge, error) {
	srcClause, srcArgs := notInClause("s.kind", excludeKinds)
	dstClause, dstArgs := notInClause("d.kind", excludeKinds)

	args := make([]any, 0, 3+len(srcArgs)+len(dstArgs))
	args = append(args, repoID, branch, minScore)
	args = append(args, srcArgs...)
	args = append(args, dstArgs...)

	query := `SELECT e.score,
		s.node_id, s.repo_id, s.symbol_path, s.file_path, s.kind, COALESCE(s.line_start, 0), COALESCE(s.line_end, 0),
		d.node_id, d.repo_id, d.symbol_path, d.file_path, d.kind, COALESCE(d.line_start, 0), COALESCE(d.line_end, 0)
		FROM edges e
		JOIN nodes s ON s.node_id = e.src_node_id AND s.branch = e.branch
		JOIN nodes d ON d.node_id = e.dst_node_id AND d.branch = e.branch
		WHERE e.repo_id = ? AND e.branch = ? AND e.kind = 'SIMILAR_TO'
		  AND e.score IS NOT NULL AND e.score >= ?` + srcClause + dstClause

	rows, err := r.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("clone_repo: similar edges query: %w", err)
	}
	defer rows.Close()

	out := make([]duplicates.SimilarEdge, 0)
	for rows.Next() {
		var e duplicates.SimilarEdge
		if err := rows.Scan(&e.Score,
			&e.Src.NodeID, &e.Src.RepoID, &e.Src.SymbolPath, &e.Src.FilePath, &e.Src.Kind, &e.Src.LineStart, &e.Src.LineEnd,
			&e.Dst.NodeID, &e.Dst.RepoID, &e.Dst.SymbolPath, &e.Dst.FilePath, &e.Dst.Kind, &e.Dst.LineStart, &e.Dst.LineEnd,
		); err != nil {
			return nil, fmt.Errorf("clone_repo: similar edges scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clone_repo: similar edges iterate: %w", err)
	}
	return out, nil
}

// cloneScopeClause builds the shared per-query-level predicate for the clone
// projections: branch (always), repo_id (only when CloneQuery.RepoID is set — an
// empty RepoID means cross-repo), an optional file_path prefix, and the kind
// exclusion. Returning the clause + its args lets ClonedNodes/StructuralNodes
// apply the SAME filter at both the outer and the GROUP BY subquery level so the
// COUNT reflects only eligible nodes.
func cloneScopeClause(q duplicates.CloneQuery, excludeKinds []string) (string, []any) {
	clause := "branch = ?"
	args := []any{q.Branch}
	if q.RepoID != "" {
		clause += " AND repo_id = ?"
		args = append(args, q.RepoID)
	}
	if q.PathPrefix != "" {
		clause += " AND file_path LIKE ?"
		args = append(args, q.PathPrefix+"%")
	}
	kindClause, kindArgs := notInClause("kind", excludeKinds)
	clause += kindClause
	args = append(args, kindArgs...)
	return clause, args
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

// Compile-time checks that *CloneRepo satisfies the consumer-owned ports.
var (
	_ duplicates.CloneStore = (*CloneRepo)(nil)
	_ duplicates.NearStore  = (*CloneRepo)(nil)
)
