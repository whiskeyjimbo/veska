// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ucSrcLine returns the source line if it is non-zero, or NULL otherwise.
func ucSrcLine(uc domain.UnresolvedCall) any {
	if uc.SrcLine <= 0 {
		return nil
	}
	return uc.SrcLine
}

// ucEdgeKind returns the target EdgeKind for the call site, defaulting to EdgeCalls.
func ucEdgeKind(uc domain.UnresolvedCall) domain.EdgeKind {
	if uc.EdgeKind == "" {
		return domain.EdgeCalls
	}
	return uc.EdgeKind
}

// stubSymbolKey derives a namespaced symbol component for a cross-repository
// stub ID to prevent key collisions for different call shapes on the same branch.
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

// buildPackageSymbolMap groups symbol names to node IDs by file directory.
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

// moduleRelDir returns the slash-separated directory path relative to the root.
// Normalizing paths ensures consistent package scoping during resolution.
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

// modulePackageDir maps a Go import path to its package directory relative to the module root.
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

// buildModuleRelSymbolMap groups symbol names by module-relative package directories.
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

// findInBatchMethod resolves method calls by receiver suffix, returning an
// empty ID and true if multiple receiver types declare the same method name.
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
