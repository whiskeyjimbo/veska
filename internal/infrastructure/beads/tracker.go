// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package beads provides a Tracker implementation backed by the beads
// file-based issue tracker. The active task ID is read from the
// beads/current_task file that beads writes into the repository root.
package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// currentTaskFile specifies the relative path from the repository root where the active task ID is stored.
const currentTaskFile = ".beads/current_task"

// FileTracker reads the active task ID from a .beads/current_task file at the repository root.
type FileTracker struct{}

var _ ports.Tracker = (*FileTracker)(nil)

func NewFileTracker() *FileTracker {
	return &FileTracker{}
}

// ActiveTask retrieves the active task by reading from the .beads/current_task file.
// If the file is missing or empty, it returns nil without an error, indicating no active task.
func (t *FileTracker) ActiveTask(_ context.Context, repoID string) (*ports.TaskSummary, error) {
	raw, err := os.ReadFile(filepath.Join(repoID, currentTaskFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	id := strings.TrimSpace(string(raw))
	if id == "" {
		return nil, nil
	}

	return &ports.TaskSummary{
		ID:        id,
		RepoID:    repoID,
		Active:    true,
		CreatedAt: time.Time{}, // File does not record creation time.
	}, nil
}

// RecentTasks is not supported by the file-based tracker and always returns nil.
func (t *FileTracker) RecentTasks(_ context.Context, _ string, _ int) ([]ports.TaskSummary, error) {
	return nil, nil
}
