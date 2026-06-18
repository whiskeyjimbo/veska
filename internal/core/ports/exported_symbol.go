// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// ExportedSymbol is the projection needed by the breaking-removal diff gate
// to determine if an exported symbol from base-ref has disappeared. It carries
// node ID and identity fields (file path, kind, name) that are folded into a
// package-scoped key so that intra-package file moves are not mistaken for removals.
type ExportedSymbol struct {
	NodeID   string
	FilePath string
	Kind     string
	Name     string
}

// ExportedSymbolQuerier is the read-side port the breaking-removal gate uses to
// enumerate exported public-surface symbols over a set of changed files.
// ExportedSymbolsInFiles returns the nodes in (repoID, branch) whose file path
// is in filePaths, whose exported flag is true, and whose kind is removable
// (e.g., function, method, interface, struct, type, variable, class). This is
// wider than the contract-drift gate's signature-shaped set because removal
// detection only requires name presence, covering exported types, structs, constants,
// and variables. An empty filePaths slice must return an empty result with no error.
type ExportedSymbolQuerier interface {
	ExportedSymbolsInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ExportedSymbol, error)
}
