// Package beads provides a Tracker implementation backed by the beads
// file-based issue tracker. The active task ID is read from the
// .beads/current_task file that beads writes into the repository root.
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

// currentTaskFile is the relative path (from the repo root) that beads writes
// the active task ID into.
const currentTaskFile = ".beads/current_task"

// FileTracker is a Tracker that reads the active task ID from a
// .beads/current_task file located at the root of the given repository
// directory. It satisfies ports.Tracker.
//
// FileTracker is safe for concurrent use.
type FileTracker struct{}

// Compile-time interface satisfaction check.
var _ ports.Tracker = (*FileTracker)(nil)

// NewFileTracker constructs a FileTracker.
func NewFileTracker() *FileTracker {
	return &FileTracker{}
}

// ActiveTask reads .beads/current_task from repoID (treated as a filesystem
// path) and returns a Task whose ID is the trimmed file contents. Returns nil,
// nil when the file is absent or empty — that is the normal state when no task
// is active.
func (t *FileTracker) ActiveTask(_ context.Context, repoID string) (*ports.Task, error) {
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

	return &ports.Task{
		ID:        id,
		RepoID:    repoID,
		Active:    true,
		CreatedAt: time.Time{}, // file does not record creation time
	}, nil
}

// RecentTasks is not supported by the file-based tracker; it always returns an
// empty slice. Use a richer Tracker implementation (e.g. beads HTTP API) to
// query task history.
func (t *FileTracker) RecentTasks(_ context.Context, _ string, _ int) ([]ports.Task, error) {
	return nil, nil
}
