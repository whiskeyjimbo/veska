// SPDX-License-Identifier: AGPL-3.0-only

// Package fts holds the async full-text-search reindex lane. The expensive FTS5
// inserts used to run co-transactionally inside the promote tx (~42s of an
// 844-file cold scan); they now run here, one queue row per promoted file, off
// the promote critical path. Orphan cleanup for deleted/renamed symbols still
// happens synchronously during promotion (sqlite.ftsSink.BeforeNodeDelete),
// because it can only identify a file's old rows while the old node rows exist.
package fts

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Reindexer rebuilds the FTS rows for one file from its current nodes. The
// consumer-owned narrow port; sqlite.FTSReindexRepo satisfies it.
type Reindexer interface {
	ReindexFile(ctx context.Context, repoID, branch, filePath string) error
}

// ErrMissingDependency is returned by NewHandler when the Reindexer is nil, so
// a wiring fault surfaces at construction rather than on the first queue row.
var ErrMissingDependency = errors.New("fts: missing required dependency")

// Handler drains WorkKindFTS rows by reindexing the row's file.
type Handler struct {
	repo Reindexer
}

// NewHandler constructs the FTS work handler. repo is required.
func NewHandler(repo Reindexer) (*Handler, error) {
	if repo == nil {
		return nil, fmt.Errorf("fts.NewHandler: repo: %w", ErrMissingDependency)
	}
	return &Handler{repo: repo}, nil
}

// Handle reindexes the file named in the row's payload. An empty payload is a
// no-op (the lane is per-file; there is no repo-scoped FTS row). A non-nil
// error tells the poller to retry, which is safe: ReindexFile is idempotent.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindFTS {
		return fmt.Errorf("fts.Handle: unexpected kind %q", row.Kind)
	}
	if row.Payload == "" {
		return nil
	}
	if err := h.repo.ReindexFile(ctx, row.RepoID, row.Branch, row.Payload); err != nil {
		return fmt.Errorf("fts.Handle: reindex %q: %w", row.Payload, err)
	}
	return nil
}
