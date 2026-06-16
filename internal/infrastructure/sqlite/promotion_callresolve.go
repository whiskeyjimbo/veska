package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// promotedScope aggregates repo, branch, root, and relative directory paths
// to simplify helper function signatures for graph lookups.
type promotedScope struct {
	repoID string
	branch string
	root   string
	relDir string
}

// lookupPromotedMethodInDir searches for a promoted method by name in a package directory,
// returning false if no match is found or if multiple receiver types declare the same method name.
func lookupPromotedMethodInDir(ctx context.Context, tx *sql.Tx, scope promotedScope, methodName string) (domain.NodeID, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, file_path FROM nodes
		   WHERE repo_id = ? AND branch = ? AND kind = 'method' AND symbol_path LIKE ?`,
		scope.repoID, scope.branch, "%."+methodName,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	// Non-test candidates are preferred because test files commonly implement
	// interface stubs that share method names with production types. If a
	// production match is found, it is returned; test matches are only checked if
	// no production matches exist.
	var prodMatch, testMatch domain.NodeID
	prodCount, testCount := 0, 0
	for rows.Next() {
		var nodeID, filePath string
		if err := rows.Scan(&nodeID, &filePath); err != nil {
			return "", false, err
		}
		if moduleRelDir(filePath, scope.root) != scope.relDir {
			continue
		}
		if strings.HasSuffix(filePath, "_test.go") {
			testMatch = domain.NodeID(nodeID)
			testCount++
			continue
		}
		prodMatch = domain.NodeID(nodeID)
		prodCount++
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	switch {
	case prodCount == 1:
		return prodMatch, true, nil
	case prodCount == 0 && testCount == 1:
		return testMatch, true, nil
	}
	return "", false, nil
}

// lookupPromotedSymbolDir finds the already-promoted node for a symbol in a
// package directory. It filters by symbol path and disambiguates by directory
// in memory to support absolute or relative file paths. The SQL rows are fully
// drained before returning to prevent write deadlocks on the transaction.
func lookupPromotedSymbolDir(ctx context.Context, tx *sql.Tx, scope promotedScope, name string) (domain.NodeID, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT node_id, file_path FROM nodes
		   WHERE repo_id = ? AND branch = ? AND symbol_path = ?`,
		scope.repoID, scope.branch, name,
	)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()

	var match domain.NodeID
	found := false
	for rows.Next() {
		var nodeID, filePath string
		if err := rows.Scan(&nodeID, &filePath); err != nil {
			return "", false, err
		}
		if found {
			continue // drain remaining rows; ambiguity handled below
		}
		if moduleRelDir(filePath, scope.root) == scope.relDir {
			match = domain.NodeID(nodeID)
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return match, found, nil
}

// isExternalModulePath reports whether an import path represents an external
// module rather than a standard library package. Only external modules can
// resolve to other registered repositories.
func isExternalModulePath(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return strings.Contains(first, ".")
}

// stubID derives a deterministic ID for a cross-repo edge stub to ensure that
// re-promoting the same call is an idempotent operation.
func stubID(srcNodeID, modulePath, symbol string) string {
	h := sha256.Sum256([]byte(srcNodeID + "\x00" + modulePath + "\x00" + symbol))
	return hex.EncodeToString(h[:])
}

func buildCallEdge(uc domain.UnresolvedCall, targetID domain.NodeID) (*domain.Edge, bool) {
	opts := []domain.EdgeOption{domain.WithConfidence(domain.Probable)}
	if uc.SrcLine > 0 {
		opts = append(opts, domain.WithSourceLine(uc.SrcLine))
	}
	e, err := domain.NewEdge(domain.EdgeSpec{
		Src:  uc.CallerID,
		Tgt:  targetID,
		Kind: ucEdgeKind(uc),
	}, opts...)
	if err != nil {
		return nil, false
	}
	return e, true
}

// resolveIntraPackageCalls resolves calls between different files in the same
// package, leaving package-qualified calls for the cross-package pass.
func (p *promotion) resolveIntraPackageCalls(ctx context.Context) error {
	pkgMaps := buildPackageSymbolMap(p.batch)
	for _, file := range p.batch.Files {
		if len(file.UnresolvedCalls) == 0 {
			continue
		}
		// Empty symbol names are allowed because the promoted-graph fallback
		// resolves calls to unchanged sibling files.
		names := pkgMaps[filepath.Dir(file.Path)]
		for _, uc := range file.UnresolvedCalls {
			// Package-qualified calls are bypassed here to prevent them from
			// binding to local symbols of the same name.
			if uc.PkgQualifier != "" {
				continue
			}
			targetID, found, err := p.lookupIntraPackageTarget(ctx, names, file, uc)
			if err != nil {
				return err
			}
			if !found || uc.CallerID == targetID {
				continue
			}
			e, ok := buildCallEdge(uc, targetID)
			if !ok {
				continue
			}
			if err := p.insertEdge(ctx, e); err != nil {
				return fmt.Errorf("promoter: insert cross-file edge %q: %w", e.ID, err)
			}
		}
	}
	return nil
}

// lookupIntraPackageTarget resolves package-local calls against the batch or the
// promoted graph, allowing incremental commits to bind to unchanged files.
// Method calls are skipped here because the batch map keys nodes by qualified
// name, meaning bare method names cannot be resolved.
func (p *promotion) lookupIntraPackageTarget(ctx context.Context, names map[string]domain.NodeID, file application.PromotionFile, uc domain.UnresolvedCall) (domain.NodeID, bool, error) {
	if tid, ok := names[uc.CalleeName]; ok {
		return tid, true, nil
	}
	if uc.IsMethodCall {
		return "", false, nil
	}
	// Fall back to the promoted graph, scoped to the caller's own package dir.
	// The cursor fully drains before any edge insert (lookupPromotedSymbolDir
	// guarantees this) so the single write connection never deadlocks.
	scope := promotedScope{repoID: p.repoID, branch: p.branch, root: p.rootPath, relDir: moduleRelDir(file.Path, p.rootPath)}
	tid, found, err := lookupPromotedSymbolDir(ctx, p.tx, scope, uc.CalleeName)
	if err != nil {
		return "", false, fmt.Errorf("promoter: intra-package lookup %q: %w", uc.CalleeName, err)
	}
	return tid, found, nil
}

// resolveCrossPackageCalls resolves package-qualified calls within the same module
// and records stubs for external cross-repository calls.
func (p *promotion) resolveCrossPackageCalls(ctx context.Context) error {
	if p.modulePath == "" {
		return nil
	}
	// Prepares the stub statement lazily to avoid touching the database table
	// for repositories without a defined module path.
	stubStmt, err := prepare(ctx, p.tx, "stub insert", `
		INSERT INTO cross_repo_edge_stubs
			(stub_id, branch, repo_id, src_node_id, kind, module_path, symbol_path, language, last_promoted_at, method_call, src_line)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stub_id, branch) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stubStmt.Close()

	byPkgDir := buildModuleRelSymbolMap(p.batch, p.rootPath)
	for _, file := range p.batch.Files {
		if len(file.UnresolvedCalls) == 0 || len(file.Imports) == 0 {
			continue
		}
		for _, uc := range file.UnresolvedCalls {
			if uc.PkgQualifier == "" {
				continue
			}
			if err := p.resolveQualifiedCall(ctx, stubStmt, byPkgDir, file, uc); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveQualifiedCall resolves the package qualifier to an import path, then
// writes a local edge or records a cross-repository stub.
func (p *promotion) resolveQualifiedCall(ctx context.Context, stubStmt *sql.Stmt, byPkgDir map[string]map[string]domain.NodeID, file application.PromotionFile, uc domain.UnresolvedCall) error {
	importPath, ok := file.Imports[uc.PkgQualifier]
	if !ok {
		// Fall back to matching the package name against registered modules if the
		// import path suffix differs from the package declaration name.
		ip, matched, err := findImportByPackageName(ctx, p.tx, file.Imports, uc.PkgQualifier)
		if err != nil {
			return fmt.Errorf("promoter: cross-repo pkg-name lookup %q: %w", uc.PkgQualifier, err)
		}
		if !matched {
			return nil // qualifier is a local var, an unregistered dep, or stdlib
		}
		importPath = ip
	}
	relDir, inModule := modulePackageDir(p.modulePath, importPath)
	if !inModule {
		return p.emitCrossRepoStub(ctx, stubStmt, uc, importPath)
	}
	return p.resolveInModuleCall(ctx, byPkgDir, uc, relDir)
}

// emitCrossRepoStub inserts a stub record for an external module call. Standard
// library calls are ignored since they cannot resolve to other repositories.
func (p *promotion) emitCrossRepoStub(ctx context.Context, stubStmt *sql.Stmt, uc domain.UnresolvedCall, importPath string) error {
	if !isExternalModulePath(importPath) {
		return nil
	}
	methodFlag := 0
	if uc.IsMethodCall {
		methodFlag = 1
	}
	kind := ucEdgeKind(uc)
	sid := stubID(string(uc.CallerID), importPath, stubSymbolKey(uc, kind))
	if _, err := stubStmt.ExecContext(ctx,
		sid, p.branch, p.repoID, string(uc.CallerID), string(kind),
		importPath, uc.CalleeName, "go", p.now, methodFlag,
		ucSrcLine(uc),
	); err != nil {
		return fmt.Errorf("promoter: insert cross-repo stub %q: %w", sid, err)
	}
	return nil
}

// resolveInModuleCall binds an in-module package-qualified call to its target node.
func (p *promotion) resolveInModuleCall(ctx context.Context, byPkgDir map[string]map[string]domain.NodeID, uc domain.UnresolvedCall, relDir string) error {
	targetID, found, err := p.lookupInModuleTarget(ctx, byPkgDir, relDir, uc)
	if err != nil {
		return err
	}
	if !found || uc.CallerID == targetID {
		return nil
	}
	e, ok := buildCallEdge(uc, targetID)
	if !ok {
		return nil
	}
	if err := p.insertEdge(ctx, e); err != nil {
		return fmt.Errorf("promoter: insert cross-package edge %q: %w", e.ID, err)
	}
	return nil
}

// lookupInModuleTarget finds the callee node for an in-module call. If the call
// is a method call, it is matched by the receiver type name suffix, skipping
// ambiguous matches to avoid false edges.
func (p *promotion) lookupInModuleTarget(ctx context.Context, byPkgDir map[string]map[string]domain.NodeID, relDir string, uc domain.UnresolvedCall) (domain.NodeID, bool, error) {
	scope := promotedScope{repoID: p.repoID, branch: p.branch, root: p.rootPath, relDir: relDir}
	if uc.IsMethodCall {
		if tid, ok := findInBatchMethod(byPkgDir, relDir, uc.CalleeName); ok {
			return tid, true, nil
		}
		tid, found, err := lookupPromotedMethodInDir(ctx, p.tx, scope, uc.CalleeName)
		if err != nil {
			return "", false, fmt.Errorf("promoter: method-call lookup %q: %w", uc.CalleeName, err)
		}
		return tid, found, nil
	}
	if tid, ok := byPkgDir[relDir][uc.CalleeName]; ok {
		return tid, true, nil
	}
	// Fall back to the promoted graph if the callee is not in this batch. The
	// cursor must be fully drained before inserting edges to avoid write deadlocks.
	tid, found, err := lookupPromotedSymbolDir(ctx, p.tx, scope, uc.CalleeName)
	if err != nil {
		return "", false, fmt.Errorf("promoter: cross-package lookup %q: %w", uc.CalleeName, err)
	}
	return tid, found, nil
}

// findImportByPackageName finds a registered module whose package node matches
// the qualifier when the import path suffix differs from the package name. If
// multiple registered modules match, it returns false to avoid creating an
// ambiguous cross-repository stub.
func findImportByPackageName(ctx context.Context, tx *sql.Tx, imports map[string]string, pkgName string) (string, bool, error) {
	if pkgName == "" || len(imports) == 0 {
		return "", false, nil
	}
	// Only external module paths are evaluated to keep the SQL parameter list small.
	paths := make([]string, 0, len(imports))
	args := []any{pkgName}
	for _, p := range imports {
		if !isExternalModulePath(p) {
			continue
		}
		paths = append(paths, p)
		args = append(args, p)
	}
	if len(paths) == 0 {
		return "", false, nil
	}
	placeholders := strings.Repeat("?,", len(paths))
	placeholders = placeholders[:len(placeholders)-1]
	q := `
		SELECT DISTINCT r.module_path
		FROM nodes n
		JOIN repos r ON r.repo_id = n.repo_id
		WHERE n.kind = 'package' AND n.symbol_path = ?
		  AND r.module_path IN (` + placeholders + `)`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return "", false, fmt.Errorf("find import by pkg name: %w", err)
	}
	defer rows.Close()
	var match string
	count := 0
	for rows.Next() {
		var mp string
		if scanErr := rows.Scan(&mp); scanErr != nil {
			return "", false, fmt.Errorf("find import by pkg name: scan: %w", scanErr)
		}
		match = mp
		count++
		if count > 1 {
			return "", false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("find import by pkg name: iter: %w", err)
	}
	if count == 1 {
		return match, true, nil
	}
	return "", false, nil
}
