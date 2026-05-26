// DependenciesRepo backs dependencies.StubAggregator by SELECTing from
// cross_repo_edge_stubs. The stub table is the canonical signal for
// external module usage (every package-qualified call to a non-stdlib
// import emits a row at promotion time — see promotion_store.go), so
// aggregating it gives accurate per-module usage counts without
// re-parsing go.mod (solov2-jlws).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
)

// DependenciesRepo is the SQLite-backed adapter for
// dependencies.StubAggregator.
type DependenciesRepo struct {
	readDB *sql.DB
}

// NewDependenciesRepo constructs a DependenciesRepo bound to readDB.
func NewDependenciesRepo(readDB *sql.DB) *DependenciesRepo {
	return &DependenciesRepo{readDB: readDB}
}

// AggregateStubs returns one row per cross_repo_edge_stub in
// (repoID, branch). Ordered by src_node_id for deterministic
// "first TopK call sites" sampling on the application side.
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
