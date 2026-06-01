package sqlite

import (
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// Pure symbol/module-path mapping helpers used by the call-resolution phases
// in promotion_callresolve.go: package/module symbol maps and in-batch method
// lookup. Split out to keep promotion_callresolve.go focused on the stateful
// (*promotion) resolution phases.

// ucSrcLine returns the SQL bind value for cross_repo_edge_stubs.src_line
// — the UnresolvedCall's SrcLine when non-zero, NULL otherwise. The
// stub-resolution step in graph_repo (cross-repo stub → resolved Edge)
// copies this value into the resulting Edge.SourceLine (solov2-izh6.31).
func ucSrcLine(uc domain.UnresolvedCall) any {
	if uc.SrcLine <= 0 {
		return nil
	}
	return uc.SrcLine
}

// ucEdgeKind returns the edge kind a resolved call site should emit. The
// zero value defaults to EdgeCalls so ordinary call sites are unchanged;
// the framework route extractor sets EdgeRoutes (solov2-ketg).
func ucEdgeKind(uc domain.UnresolvedCall) domain.EdgeKind {
	if uc.EdgeKind == "" {
		return domain.EdgeCalls
	}
	return uc.EdgeKind
}

// stubSymbolKey derives the symbol component of a cross-repo stub_id,
// namespaced so distinct call shapes from the same caller into the same
// module can't collide on the ON CONFLICT(stub_id, branch) key. A method
// call ("v.Method") and a plain call ("Method") share a name but differ in
// shape; likewise a ROUTES route→handler reference and a CALLS reference.
// CALLS keeps the bare name for backward-compatible stub_ids (solov2-ketg).
func stubSymbolKey(uc domain.UnresolvedCall, kind domain.EdgeKind) string {
	key := uc.CalleeName
	if uc.IsMethodCall {
		key = "@method:" + key
	}
	if kind != domain.EdgeCalls {
		key = "@" + string(kind) + ":" + key
	}
	return key
}

// buildPackageSymbolMap groups symbol-name → node_id by file directory.
// Go's "one package per directory" convention means a single map per
// dir is sufficient for resolving same-package, cross-file calls
// . The values shadow on conflict (last file wins) — only
// matters when two files in the same dir export the same symbol name,
// which is illegal Go anyway.
func buildPackageSymbolMap(batch application.PromotionBatch) map[string]map[string]domain.NodeID {
	out := make(map[string]map[string]domain.NodeID)
	for _, file := range batch.Files {
		dir := filepath.Dir(file.Path)
		bucket, ok := out[dir]
		if !ok {
			bucket = make(map[string]domain.NodeID)
			out[dir] = bucket
		}
		for _, n := range file.Nodes {
			if n == nil {
				continue
			}
			bucket[n.Name] = n.ID
		}
	}
	return out
}

// moduleRelDir returns path's directory relative to the repo's working-tree
// root, in slash form. Node/file paths reach promotion in a mix of absolute
// (cold scan) and repo-relative (incremental commit) forms; normalising both
// against root gives a single package-key space for cross-package resolution
// . The module-root package maps to "".
func moduleRelDir(path, root string) string {
	p := filepath.ToSlash(path)
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

// modulePackageDir maps a Go import path to its package directory relative to
// the module root. inModule is false when importPath is not under modulePath
// (stdlib or another module — handled as a cross-repo stub instead).
func modulePackageDir(modulePath, importPath string) (relDir string, inModule bool) {
	if modulePath == "" {
		return "", false
	}
	if importPath == modulePath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(importPath, modulePath+"/"); ok {
		return rest, true
	}
	return "", false
}

// buildModuleRelSymbolMap groups batch symbol names by their module-relative
// package directory (see moduleRelDir), the key space cross-package resolution
// uses. Last writer wins on name conflict — illegal within one Go package.
func buildModuleRelSymbolMap(batch application.PromotionBatch, root string) map[string]map[string]domain.NodeID {
	out := make(map[string]map[string]domain.NodeID)
	for _, file := range batch.Files {
		dir := moduleRelDir(file.Path, root)
		bucket, ok := out[dir]
		if !ok {
			bucket = make(map[string]domain.NodeID)
			out[dir] = bucket
		}
		for _, n := range file.Nodes {
			if n != nil {
				bucket[n.Name] = n.ID
			}
		}
	}
	return out
}

// findInBatchMethod walks the per-pkg-dir bucket looking for any method
// whose bare name (the suffix after "Receiver.") equals methodName.
// Returns ("", false) on no match; returns ("", true) [empty id, found=true]
// on ambiguity (multiple receiver types own a method with that name).
// solov2-9rc2: lets the promotion-time resolver bind chained-selector
// calls like `v := pkg.New(...); v.Method()` to the method in pkg, where
// the receiver type is unknown to the parser.
func findInBatchMethod(byPkgDir map[string]map[string]domain.NodeID, relDir, methodName string) (domain.NodeID, bool) {
	bucket, ok := byPkgDir[relDir]
	if !ok {
		return "", false
	}
	suffix := "." + methodName
	var match domain.NodeID
	count := 0
	for name, id := range bucket {
		if strings.HasSuffix(name, suffix) {
			match = id
			count++
		}
	}
	if count == 1 {
		return match, true
	}
	return "", false
}
