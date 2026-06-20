// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
)

// DependenciesRepo implements dependencies.StubAggregator using a SQLite database.
type DependenciesRepo struct {
	readDB *sql.DB
}

func NewDependenciesRepo(readDB *sql.DB) *DependenciesRepo {
	return &DependenciesRepo{readDB: readDB}
}

// ListImports returns all file-import associations in (repoID, branch).
func (r *DependenciesRepo) ListImports(ctx context.Context, repoID, branch string) ([]dependencies.ImportRow, error) {
	const query = `
		SELECT file_path, import_path, language
		FROM file_imports
		WHERE repo_id = ? AND branch = ?
		ORDER BY file_path, import_path`

	rows, err := r.readDB.QueryContext(ctx, query, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("dependencies_repo: query imports: %w", err)
	}
	defer rows.Close()

	var out []dependencies.ImportRow
	for rows.Next() {
		var i dependencies.ImportRow
		if err := rows.Scan(&i.FilePath, &i.ImportPath, &i.Language); err != nil {
			return nil, fmt.Errorf("dependencies_repo: scan imports: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dependencies_repo: iterate imports: %w", err)
	}
	return out, nil
}

// AggregateStubs returns cross-repository edge stubs in (repoID, branch).
func (r *DependenciesRepo) AggregateStubs(ctx context.Context, repoID, branch string) ([]dependencies.StubRow, error) {
	const query = `
		SELECT module_path, symbol_path, src_node_id, language
		FROM cross_repo_edge_stubs
		WHERE repo_id = ? AND branch = ?
		ORDER BY src_node_id`

	rows, err := r.readDB.QueryContext(ctx, query, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("dependencies_repo: query stubs: %w", err)
	}
	defer rows.Close()

	var out []dependencies.StubRow
	for rows.Next() {
		var s dependencies.StubRow
		if err := rows.Scan(&s.ModulePath, &s.SymbolPath, &s.SrcNodeID, &s.Language); err != nil {
			return nil, fmt.Errorf("dependencies_repo: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dependencies_repo: iterate: %w", err)
	}
	return out, nil
}
