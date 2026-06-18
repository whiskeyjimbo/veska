// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// TodoEntry represents a single TODO finding.
type TodoEntry struct {
	FindingID string
	RepoID    string
	Branch    string
	FilePath  string
	Message   string
	State     string
	CreatedAt int64
}

// TodoQuerier is distinct from FindingStorage to avoid coupling database
// storage ports with display concerns.
type TodoQuerier interface {
	FindTodos(ctx context.Context, repoID, branch string, onlyOpen bool) ([]TodoEntry, error)
}
