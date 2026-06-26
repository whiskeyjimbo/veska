// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
)

// PackageDepsRepo aggregates the per-file internal import rows in file_imports
// into a package-level dependency graph (importing package dir -> imported
// package dirs, module-relative). It is the read-side substrate shared by the
// import-cycle and layering checks; emitting at the file grain keeps the data
// incrementally maintained, while aggregation to packages happens here at read
// time so there are no stale package-level edges to reconcile.
type PackageDepsRepo struct {
	readDB *sql.DB
}

// NewPackageDepsRepo constructs a PackageDepsRepo over a read-only connection.
func NewPackageDepsRepo(readDB *sql.DB) *PackageDepsRepo {
	return &PackageDepsRepo{readDB: readDB}
}

// pkgImportRow is one internal import as stored: the importing file and the
// imported (intra-module) package path.
type pkgImportRow struct {
	filePath   string
	importPath string
}

// PackageDependencies returns the module-internal package dependency graph for
// (repoID, branch) as an adjacency map of importer package dir -> sorted unique
// imported package dirs (all module-relative, e.g. "internal/core/domain").
// Self-edges are dropped. An empty graph (no internal imports yet) is not an
// error.
func (r *PackageDepsRepo) PackageDependencies(ctx context.Context, repoID, branch string) (map[string][]string, error) {
	var modulePath sql.NullString
	err := r.readDB.QueryRowContext(ctx,
		`SELECT module_path FROM repos WHERE repo_id = ?`, repoID).Scan(&modulePath)
	if err == sql.ErrNoRows {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("package_deps: lookup module path: %w", err)
	}

	rows, err := r.readDB.QueryContext(ctx,
		`SELECT file_path, import_path FROM file_imports
		 WHERE repo_id = ? AND branch = ? AND internal = 1`,
		repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("package_deps: query internal imports: %w", err)
	}
	defer rows.Close()

	var imports []pkgImportRow
	for rows.Next() {
		var ir pkgImportRow
		if err := rows.Scan(&ir.filePath, &ir.importPath); err != nil {
			return nil, fmt.Errorf("package_deps: scan: %w", err)
		}
		imports = append(imports, ir)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("package_deps: iterate: %w", err)
	}
	return aggregatePackageDeps(imports, modulePath.String), nil
}

// aggregatePackageDeps folds per-file internal imports into a package adjacency
// map. It is pure (no DB) so the resolution logic is unit-testable: each row's
// importer package is the directory of its file, and the imported package is the
// import path made module-relative via modulePackageDir. Rows whose import path
// is not inside the module, and self-edges, are skipped.
func aggregatePackageDeps(imports []pkgImportRow, modulePath string) map[string][]string {
	sets := make(map[string]map[string]struct{})
	for _, ir := range imports {
		// Test files are excluded: Go keeps _test.go imports out of the
		// build-time package import graph, so a "cycle" that only closes
		// through test imports (e.g. embedder_test.go -> sqlite, while sqlite
		// -> application in prod) compiles fine and is not a real import cycle.
		// Including them produces false-positive cycle findings.
		if isGoTestFile(ir.filePath) {
			continue
		}
		src := pkgDir(ir.filePath)
		dst, ok := modulePackageDir(modulePath, ir.importPath)
		if !ok || src == dst {
			continue
		}
		if sets[src] == nil {
			sets[src] = make(map[string]struct{})
		}
		sets[src][dst] = struct{}{}
	}
	out := make(map[string][]string, len(sets))
	for src, dstSet := range sets {
		dsts := make([]string, 0, len(dstSet))
		for d := range dstSet {
			dsts = append(dsts, d)
		}
		sort.Strings(dsts)
		out[src] = dsts
	}
	return out
}

// isGoTestFile reports whether a path is a Go test file, whose imports Go
// excludes from the build-time package dependency graph.
func isGoTestFile(filePath string) bool {
	return strings.HasSuffix(filePath, "_test.go")
}

// pkgDir returns the module-relative package directory of a repo-relative file
// path (file_imports stores repo-relative paths). A file at the module root maps
// to the empty-string package.
func pkgDir(filePath string) string {
	dir := path.Dir(path.Clean(filePath))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}
