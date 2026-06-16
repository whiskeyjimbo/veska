package application

import (
	"context"
	"fmt"
	"log/slog"
)

// BranchReader reads the working-tree current branch name for a repo (via
// `git symbolic-ref --short HEAD`). Returns "" for detached HEAD / no git /
// missing tree; the reconciler treats "" as "cannot determine" and skips.
type BranchReader interface {
	CurrentBranch(rootPath string) (string, error)
}

// ActiveBranchStore reads and writes repos.active_branch for a repo.
type ActiveBranchStore interface {
	ActiveBranch(ctx context.Context, repoID string) (string, error)
	SetActiveBranch(ctx context.Context, repoID, branch string) error
}

// GenerationBumper bumps the staging generation counter so in-flight saves
// carrying the old generation are discarded. Satisfied by *staging.Gate.
type GenerationBumper interface {
	BumpGeneration() uint64
}

// StagingClearer drops all staged state for (repoID, branch). Satisfied by
// *staging.Area.
type StagingClearer interface {
	Clear(repoID, branch string)
}

// BranchReconciler closes the hook-discarded branch-switch case: if the working
// tree's current branch differs from repos.active_branch at sweep/restart time,
// it bumps the staging generation (before any parse runs), drops the prior
// branch's staging entries, and updates repos.active_branch.
// It is the reusable unit shared by the wake-reconcile sweep (wired now) and
// the startup-resync path (wiring deferred). The check is
// deliberately LIGHTER than staging.Gate.BranchSwitch: no Pause/drain/Resume
// quiescence — only the three primitives §5.2 calls for.
type BranchReconciler struct {
	reader  BranchReader
	store   ActiveBranchStore
	bumper  GenerationBumper
	clearer StagingClearer
}

// NewBranchReconciler wires the reconciler to its narrow consumer-owned ports.
// Returns ErrMissingDependency on any nil dependency.
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

// Reconcile compares the working-tree branch to repos.active_branch and, on
// mismatch, bumps the generation, clears the prior branch's staging, and stores
// the new branch. A branch that cannot be determined ("" or git error) is a
// no-op: a transient git failure must never wipe valid in-flight staging.
// It returns the resolved working-tree branch ONLY on a successful mismatch
// reconcile (the authoritative new branch a caller should adopt as its
// save/promote key); it returns "" on every no-op path (unreadable branch,
// detached HEAD, or branch already matching active_branch) so a flaky read
// never causes a caller to switch keys.
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
