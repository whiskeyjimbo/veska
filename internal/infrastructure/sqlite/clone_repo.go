
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
)

// CloneRepo implements duplicates.CloneStore using a SQLite database.
// A read-only database connection is used because clone queries are query-only
// and must not block the single-writer SQLite connection.
type CloneRepo struct {
	readDB *sql.DB
}

func NewCloneRepo(readDB *sql.DB) *CloneRepo {
	return &CloneRepo{readDB: readDB}
}

// ClonedNodes queries nodes sharing a content_hash within the given scope,
// filtering out empty content hashes to prevent unpopulated nodes from grouping together.
func (r *CloneRepo) ClonedNodes(ctx context.Context, q duplicates.CloneQuery, excludeKinds []string) ([]duplicates.ClonedNode, error) {
	// The scope clause is applied identically to both the outer query and the HAVING subquery
	// to prevent node counts from leaking across different repositories.
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

// StructuralNodes clusters nodes using structural_hash to identify duplicate structures
// with renamed variables, ignoring NULL values which indicate missing structural signals.
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

// SimilarEdges retrieves SIMILAR_TO edges exceeding the score threshold and
// filters out legacy edges with NULL scores.
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

// cloneScopeClause builds a shared query filter to ensure consistent criteria
// are applied across nested queries.
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

// notInClause builds a parameterized NOT IN SQL clause for list filtering.
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

var (
	_ duplicates.CloneStore = (*CloneRepo)(nil)
	_ duplicates.NearStore  = (*CloneRepo)(nil)
)
