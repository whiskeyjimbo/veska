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

// This file holds the symbol/call-resolution machinery used by the promotion
// transaction: the package/module symbol maps, the promoted-graph lookups, the
// cross-repo stub bookkeeping, and the resolveIntraPackageCalls /
// resolveCrossPackageCalls phases that bind parser UnresolvedCalls into CALLS
// edges. The core promotion transaction (node/file writes, sinks, commit) lives
// in promotion_store.go; these resolution phases are invoked from Promote's
// phase list there.

// promotedScope is the (repo, branch, root, package-dir) tuple shared by the
// promoted-graph lookups below. Bundling it keeps each lookup's signature small
// and threads the four values that always travel together as one unit.
type promotedScope struct {
	repoID string
	branch string
	root   string
	relDir string
}

// lookupPromotedMethodInDir is lookupPromotedSymbolDir's method-by-bare-name
// variant: given a method name like "Hello" and a target package dir, find
// the unique promoted method node whose symbol_path ends with ".Hello" and
// whose file lives in scope.relDir. Returns found=false on miss or on ambiguity
// (multiple receiver types own a Hello method in the same package — rare in
// well-typed Go but possible). solov2-9rc2.
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

	// solov2-9rc2: prefer non-test candidates. Test files commonly declare
	// stub implementations of an interface ("type stubX struct {}; func
	// (s *stubX) Write(...) ...") that share a method name with the
	// production type. If a production match exists, return it without
	// failing on the test-vs-production ambiguity; only when production
	// matches are themselves ambiguous (or absent) do we count test
	// matches in the disambiguation pass.
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

// lookupPromotedSymbolDir finds the already-promoted node for symbol `name`
// living in module-relative package dir `relDir`. It scans candidates by
// symbol_path (indexed) and disambiguates by directory in Go, since promoted
// file paths may be absolute or repo-relative. The cursor is fully drained
// before returning so callers may safely write on the same tx afterwards
// . found is false on no match.
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

// isExternalModulePath reports whether importPath looks like a third-party Go
// module (its first segment contains a "." — a hostname like github.com),
// rather than a standard-library package (fmt, net/http). Only external
// modules can match another registered repo, so stdlib calls get no stub.
func isExternalModulePath(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return strings.Contains(first, ".")
}

// stubID derives a deterministic id for a cross-repo edge stub from its source
// node, target module path and symbol, so re-promoting the same call is a
// no-op under the ON CONFLICT clause.
func stubID(srcNodeID, modulePath, symbol string) string {
	h := sha256.Sum256([]byte(srcNodeID + "\x00" + modulePath + "\x00" + symbol))
	return hex.EncodeToString(h[:])
}

// buildCallEdge constructs a Probable edge for a resolved call site,
// carrying the source line when known. The edge kind is ucEdgeKind(uc) —
// EdgeCalls by default, EdgeRoutes for route→handler refs (solov2-ketg).
// Returns ok=false when the domain constructor rejects the inputs.
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

// resolveIntraPackageCalls binds UnresolvedCalls whose callee lives in another
// file of the same Go package . Package-qualified calls are left to
// the cross-package pass; same-directory = same package by convention. Misses
// stay unresolved.
func (p *promotion) resolveIntraPackageCalls(ctx context.Context) error {
	pkgMaps := buildPackageSymbolMap(p.batch)
	for _, file := range p.batch.Files {
		if len(file.UnresolvedCalls) == 0 {
			continue
		}
		// names may be empty for a single-file batch (no siblings staged); the
		// promoted-graph fallback below still resolves calls into UNCHANGED
		// same-package files, so we must NOT skip on an empty in-batch map.
		names := pkgMaps[filepath.Dir(file.Path)]
		for _, uc := range file.UnresolvedCalls {
			// Package-qualified calls (cmd.Execute) bind via the import map in
			// the cross-package pass, never by bare name against the local
			// package — else a same-named local symbol would bind falsely.
			if uc.PkgQualifier != "" {
				continue
			}
			targetID, found, err := p.lookupIntraPackageTarget(ctx, names, file, uc)
			if err != nil {
				return err
			}
			if !found || uc.CallerID == targetID { // miss or self-call (recursion)
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

// lookupIntraPackageTarget resolves a same-package, bare-name call first against
// the current batch then against the already-promoted graph — the latter so an
// incremental single-file commit still binds calls into UNCHANGED sibling files
// (solov2-ll57.13; mirrors lookupInModuleTarget's cross-package fallback). Method
// calls are NOT resolved here: the in-batch map keys nodes by qualified name
// ("T.Method"), so the batch path never binds a bare-named method call intra-
// package, and the fallback stays symmetric with it by skipping them too.
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

// resolveCrossPackageCalls binds package-qualified calls within the same Go
// module and records cross-repo edge stubs for calls into other modules
// . Repos without a module_path skip the table entirely.
// Ambiguity/misses are skipped: this pass never emits a false edge.
func (p *promotion) resolveCrossPackageCalls(ctx context.Context) error {
	if p.modulePath == "" {
		return nil
	}
	// Stub statement prepared lazily here so promotions for repos without a
	// module_path never touch the table. Bound by the query-time resolver to
	// whichever registered repo owns the module_path (solov2-xc51.3 / 1gj).
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

// resolveQualifiedCall resolves the call's package qualifier to an import path
// (with the package-name fallback of solov2-izh6.6), then either binds an
// in-module CALLS edge or records a cross-repo stub.
func (p *promotion) resolveQualifiedCall(ctx context.Context, stubStmt *sql.Stmt, byPkgDir map[string]map[string]domain.NodeID, file application.PromotionFile, uc domain.UnresolvedCall) error {
	importPath, ok := file.Imports[uc.PkgQualifier]
	if !ok {
		// The import-alias key is the last URL segment (Go convention), but a
		// module's package name can diverge from its URL (e.g.
		// github.com/jrose/greetlib declares `package greet`). Fall back to a
		// registered Go module among this file's imports whose promoted package
		// node is named uc.PkgQualifier; a single match binds (solov2-izh6.6).
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

// emitCrossRepoStub records a cross-repo edge stub for a package-qualified call
// into another module. Stdlib (no domain in the first path segment) can never
// match a repo, so it is skipped to keep the table lean. solov2-9rc2 Phase C:
// plain and method calls both emit stubs, distinguished by method_call.
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

// resolveInModuleCall binds an in-module package-qualified call to its target
// node and writes the CALLS edge. Self-calls and misses are skipped.
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

// lookupInModuleTarget finds the target node for an in-module call, first in
// the current batch then in the already-promoted graph (so incremental
// single-file commits still bind). Method calls (solov2-9rc2 Phase B) carry the
// bare method name and match by `<Receiver>.<name>` suffix; single-match binds,
// ambiguity is skipped to preserve the "no false edges" invariant.
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
	// Fall back to the promoted graph (callee's file not in this batch). The
	// cursor must fully drain before the edge insert: a query open during
	// ExecContext deadlocks the single write connection.
	tid, found, err := lookupPromotedSymbolDir(ctx, p.tx, scope, uc.CalleeName)
	if err != nil {
		return "", false, fmt.Errorf("promoter: cross-package lookup %q: %w", uc.CalleeName, err)
	}
	return tid, found, nil
}

// findImportByPackageName looks for a registered Go module among the
// supplied imports whose promoted package node is named pkgName. Used as
// a fallback when the parser's import-alias key (last URL segment) does
// not match the call-site qualifier — common when a module's package
// declaration diverges from its URL (e.g. github.com/jrose/greetlib
// declares `package greet`). solov2-izh6.6.
//
// Returns the matching import path when exactly one registered module
// matches. Multiple matches are reported as "no match" rather than
// guessing — emitting a stub against the wrong module would violate the
// promoter's "no false edges" invariant; ambiguity is a real signal to
// fix the call site or add an explicit import alias upstream.
func findImportByPackageName(ctx context.Context, tx *sql.Tx, imports map[string]string, pkgName string) (string, bool, error) {
	if pkgName == "" || len(imports) == 0 {
		return "", false, nil
	}
	// Collect candidate module paths — only those that look external
	// (have a '.' in the first segment); stdlib never reverse-resolves to
	// a registered repo and we want to keep the IN-list short.
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
			return "", false, nil // ambiguous — caller will skip
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
