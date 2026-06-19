// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"context"
	"fmt"
	"log/slog"
)

// BranchReader queries the current branch name, returning an empty string on
// detached HEAD or missing directory.
type BranchReader interface {
	CurrentBranch(rootPath string) (string, error)
}

type ActiveBranchStore interface {
	ActiveBranch(ctx context.Context, repoID string) (string, error)
	SetActiveBranch(ctx context.Context, repoID, branch string) error
}

// GenerationBumper increments the staging generation count to invalidate older
// in-flight saves.
type GenerationBumper interface {
	BumpGeneration() uint64
}

type StagingClearer interface {
	Clear(repoID, branch string)
}

// BranchReconciler monitors branch changes. If the current working tree branch
// diverges from the database active branch, it invalidates staged files and
// updates active branch tracking.
type BranchReconciler struct {
	reader  BranchReader
	store   ActiveBranchStore
	bumper  GenerationBumper
	clearer StagingClearer
}

func NewBranchReconciler(
	reader BranchReader,
	store ActiveBranchStore,
	bumper GenerationBumper,
	clearer StagingClearer,
) (*BranchReconciler, error) {
	if reader == nil || store == nil || bumper == nil || clearer == nil {
		return nil, fmt.Errorf("application.NewBranchReconciler: nil dependency: %w", ErrMissingDependency)
	}
	return &BranchReconciler{reader: reader, store: store, bumper: bumper, clearer: clearer}, nil
}

// Reconcile updates the tracked branch on branch mismatches, returning the new
// branch name. Detached HEADs or read failures are ignored to prevent
// invalidating in-flight staging.
func (r *BranchReconciler) Reconcile(ctx context.Context, repoID, rootPath string) (string, error) {
	cur, err := r.reader.CurrentBranch(rootPath)
	if err != nil {
		slog.Debug("branch_reconcile: read current branch failed; skipping", "repo", repoID, "err", err)
		return "", nil
	}
	if cur == "" {
		return "", nil
	}
	prev, err := r.store.ActiveBranch(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("branch reconcile: read active branch for %s: %w", repoID, err)
	}
	if cur == prev {
		return "", nil
	}
	r.bumper.BumpGeneration()
	r.clearer.Clear(repoID, prev)
	if err := r.store.SetActiveBranch(ctx, repoID, cur); err != nil {
		return "", fmt.Errorf("branch reconcile: set active branch for %s: %w", repoID, err)
	}
	slog.Info("branch_reconcile: working tree branch changed during suspend",
		"repo", repoID, "prev", prev, "cur", cur)
	return cur, nil
}
