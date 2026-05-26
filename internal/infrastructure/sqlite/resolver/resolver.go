// Package resolver provides query-time cross-repo edge resolution for the
// veska graph store. It performs one-hop indexed lookups against the
// cross_repo_edge_stubs and nodes tables. Multi-hop traversal is deferred to M2+.
package resolver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// CrossRepoStub carries the fields needed to resolve a single cross-repo edge
// stub at query time.
type CrossRepoStub struct {
	StubID     string
	SrcNodeID  string
	Kind       string
	ModulePath string
	SymbolPath string
	Language   string
}

// ResolvedEdge is re-exported from core/ports. The canonical definition lives
// in internal/core/ports/resolved_edge.go so that application-layer consumers
// can reference it without importing this adapter package.
type ResolvedEdge = ports.ResolvedEdge

// ResolveCrossRepoEdge resolves a single stub to its destination node, if one
// is indexed. nil, nil indicates a silent miss (no registered repo owns the
// target module, or no node in the target subpackage matches the symbol).
//
// The matcher is two-step so subpackage imports of multi-package modules
// resolve (solov2-hkr9): step 1 finds the most-specific repo whose module_path
// is a prefix of stub.module_path (longest prefix wins — so import
// github.com/x/y/z prefers a repo with module_path github.com/x/y/z over one
// with github.com/x/y); step 2 looks up the symbol in that repo, constrained
// to the subpackage dir derived from the prefix gap. expandCrossRepo is
// accepted for API symmetry with M2+ multi-hop expansion; for now both values
// produce the same single-hop result.
func ResolveCrossRepoEdge(ctx context.Context, db *sql.DB, stub CrossRepoStub, expandCrossRepo bool) (*ResolvedEdge, error) {
	const repoQ = `
		SELECT repo_id, module_path, root_path, active_branch
		FROM repos
		WHERE module_path != ''
		  AND (module_path = ? OR ? LIKE module_path || '/%')
		ORDER BY LENGTH(module_path) DESC
		LIMIT 1`

	var dstRepoID, modulePath, rootPath, activeBranch string
	err := db.QueryRowContext(ctx, repoQ, stub.ModulePath, stub.ModulePath).
		Scan(&dstRepoID, &modulePath, &rootPath, &activeBranch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolver: locate repo for stub %s: %w", stub.StubID, err)
	}

	// Subpackage directory relative to the matched repo's module root.
	// Empty when the import path is the module root (leaf-package case).
	var subDir string
	if stub.ModulePath != modulePath {
		subDir = strings.TrimPrefix(stub.ModulePath, modulePath+"/")
	}

	const nodeQ = `
		SELECT node_id, branch, file_path
		FROM nodes
		WHERE repo_id = ? AND symbol_path = ? AND language = ? AND branch = ?`
	rows, err := db.QueryContext(ctx, nodeQ, dstRepoID, stub.SymbolPath, stub.Language, activeBranch)
	if err != nil {
		return nil, fmt.Errorf("resolver: lookup symbol for stub %s: %w", stub.StubID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, branch, filePath string
		if err := rows.Scan(&nodeID, &branch, &filePath); err != nil {
			return nil, fmt.Errorf("resolver: scan symbol row: %w", err)
		}
		if moduleRelDir(filePath, rootPath) != subDir {
			continue
		}
		return &ResolvedEdge{
			SrcNodeID: stub.SrcNodeID,
			DstNodeID: nodeID,
			DstRepoID: dstRepoID,
			DstBranch: branch,
			Kind:      stub.Kind,
			CrossRepo: true,
		}, nil
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolver: iterate symbol rows: %w", err)
	}
	return nil, nil
}

// moduleRelDir returns filePath's directory relative to the repo's working-tree
// root, in slash form. Promoted file_path values reach the resolver in a mix of
// absolute (cold-scan) and repo-relative (incremental promotion) forms;
// stripping a known root harmonises both into the module-relative key space the
// subpackage match needs. The module-root package maps to "".
func moduleRelDir(filePath, root string) string {
	p := filepath.ToSlash(filePath)
	if root != "" {
		if rest, ok := strings.CutPrefix(p, filepath.ToSlash(root)+"/"); ok {
			p = rest
		}
	}
	dir := filepath.ToSlash(filepath.Dir(p))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// ResolveStubsTargetingNode is the reverse of ResolveStubsForNode: given a
// node N (a potential CALLEE), it finds every cross_repo_edge_stub whose
// forward resolution would land on N, and returns one ResolvedEdge per
// match with DstNodeID=N. This makes "who calls this library symbol?"
// answerable across repo boundaries (solov2-80hh).
//
// Mechanics: stubs are keyed by (language, module_path, symbol_path) at the
// import-path level — N's import path is N.repo's module_path joined with
// N's subpackage directory (relative to the repo root). We query the
// idx_stubs_resolver index, then for each match run the forward resolver
// and keep edges whose DstNodeID equals N's node_id. Branch matching
// follows the same model as forward resolution: the stub's branch is the
// SRC repo's, the dst node carries its own branch, so we filter on the
// caller's branch arg (the branch of N's containing repo) rather than the
// stub branch.
func ResolveStubsTargetingNode(ctx context.Context, db *sql.DB, dstNodeID, branch string) ([]ResolvedEdge, error) {
	// Step 1: load N's identity (repo_id, file_path, symbol_path, language).
	const nodeQ = `
		SELECT n.symbol_path, n.language, n.file_path, n.repo_id, r.module_path, r.root_path
		FROM nodes n
		JOIN repos r ON r.repo_id = n.repo_id
		WHERE n.node_id = ? AND n.branch = ?
		LIMIT 1`
	var symPath, lang, filePath, repoID, modulePath, rootPath string
	if err := db.QueryRowContext(ctx, nodeQ, dstNodeID, branch).
		Scan(&symPath, &lang, &filePath, &repoID, &modulePath, &rootPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolver: load dst node %s@%s: %w", dstNodeID, branch, err)
	}
	if modulePath == "" {
		// Repo has no module_path indexed (non-Go module, or never set);
		// nothing can reverse-resolve to this node by import-path matching.
		return nil, nil
	}

	// N's import path = repo.module_path + ("/" + subDir if any).
	importPath := modulePath
	if subDir := moduleRelDir(filePath, rootPath); subDir != "" {
		importPath = modulePath + "/" + subDir
	}

	// Step 2: fetch candidate stubs. Filter by repo_id != N's repo so we
	// only return TRUE cross-repo callers; an intra-repo stub (rare; only
	// happens when a repo imports its own module path before promotion
	// catches up) is not a cross-repo edge for blast purposes.
	const stubQ = `
		SELECT stub_id, branch, src_node_id, kind, module_path, symbol_path, language
		FROM cross_repo_edge_stubs
		WHERE language = ? AND module_path = ? AND symbol_path = ?
		  AND repo_id != ?`
	rows, err := db.QueryContext(ctx, stubQ, lang, importPath, symPath, repoID)
	if err != nil {
		return nil, fmt.Errorf("resolver: fetch inbound stubs for node %s: %w", dstNodeID, err)
	}
	defer rows.Close()

	type candidate struct {
		stub      CrossRepoStub
		srcBranch string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.stub.StubID, &c.srcBranch, &c.stub.SrcNodeID, &c.stub.Kind, &c.stub.ModulePath, &c.stub.SymbolPath, &c.stub.Language); err != nil {
			return nil, fmt.Errorf("resolver: scan inbound stub row: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolver: iterate inbound stubs: %w", err)
	}

	// Step 3: forward-resolve each candidate and keep edges that land on
	// our target node. The forward resolver does the subpackage-dir
	// disambiguation, so module_path collisions between repos
	// (rare but possible) are correctly filtered.
	var edges []ResolvedEdge
	for _, c := range cands {
		edge, err := ResolveCrossRepoEdge(ctx, db, c.stub, false)
		if err != nil {
			return nil, err
		}
		if edge == nil || edge.DstNodeID != dstNodeID {
			continue
		}
		// Flip the perspective: blast/call_chain see this as an edge
		// pointing IN to dstNodeID. Caller (a callers-direction blast or
		// a call_chain direction=in) treats the response as inbound.
		edges = append(edges, *edge)
	}
	return edges, nil
}

// ResolveStubsForNode fetches all cross_repo_edge_stubs whose src_node_id
// matches nodeID on the given branch, then resolves each with a one-hop lookup.
// Stubs that do not resolve to an indexed node are silently dropped.
// expandCrossRepo is forwarded to ResolveCrossRepoEdge (M2+ extension point).
func ResolveStubsForNode(ctx context.Context, db *sql.DB, nodeID, branch string, expandCrossRepo bool) ([]ResolvedEdge, error) {
	const q = `
		SELECT stub_id, src_node_id, kind, module_path, symbol_path, language
		FROM cross_repo_edge_stubs
		WHERE src_node_id = ? AND branch = ?`

	rows, err := db.QueryContext(ctx, q, nodeID, branch)
	if err != nil {
		return nil, fmt.Errorf("resolver: fetch stubs for node %s@%s: %w", nodeID, branch, err)
	}
	defer rows.Close()

	var stubs []CrossRepoStub
	for rows.Next() {
		var s CrossRepoStub
		if err := rows.Scan(&s.StubID, &s.SrcNodeID, &s.Kind, &s.ModulePath, &s.SymbolPath, &s.Language); err != nil {
			return nil, fmt.Errorf("resolver: scan stub row: %w", err)
		}
		stubs = append(stubs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolver: iterate stubs: %w", err)
	}

	var edges []ResolvedEdge
	for _, stub := range stubs {
		edge, err := ResolveCrossRepoEdge(ctx, db, stub, expandCrossRepo)
		if err != nil {
			return nil, err
		}
		if edge == nil {
			continue // silent miss
		}
		edges = append(edges, *edge)
	}
	return edges, nil
}
