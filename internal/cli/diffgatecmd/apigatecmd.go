// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// APIParams are the breaking-exported-signature diff-gate inputs. Like
// clones/untested/cycles it is a blanket gate (no target finding) and needs the
// indexed base graph: the re-promote clones it, and a node's prev_signature is
// the base signature it drifts from.
type APIParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// apiGateReport is the JSON envelope: the verdict plus its failing-check names.
type apiGateReport struct {
	diffgate.APIVerdict
	Failures []string `json:"failures"`
}

// RunAPIBreak gates the candidate on a breaking exported-signature change. It
// re-promotes the candidate's changed files into a throwaway clone of the base
// graph (so each changed node's prev_signature is set to its base signature and
// signature to the candidate's), reads the drifted nodes over the changed files,
// and FAILs on any whose visibility flag is exported.
// Index-ahead safety: a node's prev_signature is the signature
// in the clone BEFORE the candidate re-promote. By cloning the after-state from
// the base-ref-pinned clone (buildPinnedEphemeral re-promotes base-ref's changed
// files) rather than the live index, prev_signature is the BASE-REF signature
// even if a daemon has advanced the index to the candidate's content. So drift
// reads base-ref-sig vs candidate-sig and a breaking change still FAILs. No
// cross-file splice is needed here (drift is per-node, not edge-derived).
func RunAPIBreak(ctx context.Context, p APIParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate api: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate api: open store: %w", err)
	}
	defer pools.Close()

	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved
	p.Branch = resolveBranch(ctx, pools.ReadDB, p.RepoID, p.Branch)

	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := apiGateReport{
			APIVerdict: diffgate.APIVerdict{Pass: false},
			Failures:   []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitAPIReport(p.Out, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (%s)", ErrGateFailed, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
	}

	// Pin base to base-ref (index-ahead hardening): drift is read
	// from an after-state clone derived from baseClonePath (not the live index),
	// so prev_signature is the base-ref signature.
	eph, changes, baseClonePath, cleanup, err := buildPinnedEphemeral(ctx, ephemeralParams{
		RepoID:       p.RepoID,
		Branch:       p.Branch,
		RepoRoot:     p.RepoRoot,
		BaseRef:      p.BaseRef,
		CandidateRef: p.CandidateRef,
	}, dbPath)
	if err != nil {
		return err
	}
	defer cleanup()

	// Base-ref exported public surface over the changed files (from the pinned
	// base clone) - the symbols whose disappearance is a breaking removal.
	baseExported, err := exportedSymbolsAt(ctx, baseClonePath, p.RepoID, p.Branch, eph.ChangedFiles)
	if err != nil {
		return fmt.Errorf("diff-gate api: base exported: %w", err)
	}

	// After-state: clone the base-ref-pinned graph, re-promote the candidate, and
	// query BOTH signals (drift + exported set) off the SAME clone.
	drifted, candExported, err := afterStateAPISignals(ctx, baseClonePath, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
	if err != nil {
		return fmt.Errorf("diff-gate api: %w", err)
	}

	verdict := diffgate.NewAPIGate().Evaluate(drifted, baseExported, candExported)

	rep := apiGateReport{APIVerdict: verdict, Failures: verdict.Failures()}
	if err := emitAPIReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// afterStateAPISignals re-promotes the candidate's changed files into a
// throwaway clone of the (base-ref-pinned) base graph and reads BOTH api-gate
// signals off that single clone:
//
//	drifted: contract-drift rows over the changed files (nodes whose
//	  prev_signature != signature). The re-promote is what sets prev_signature
//	  (base-ref) vs signature (candidate); the drift query is self-scoping to
//	  genuinely-changed signatures. Visibility filtering (exported-only) lives in
//	  the gate, leaving the whole-repo contract-drift querier unchanged.
//	candidateExported: the exported public-surface symbols over the changed
//	  files in the candidate after-state, for the removal/rename detector.
//
// Both reads hit the same clone so the DB is cloned and re-promoted once.
//
//nolint:revive // argument-limit: intrinsic clone+identity+change plumbing, matching the sibling diff-gate helpers (untestedInChangedFiles, cycleEdgeGraphs); not a cohesive struct.
func afterStateAPISignals(ctx context.Context, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, changedFiles []string) ([]ports.DriftedNode, []ports.ExportedSymbol, error) {
	clone, err := cloneDB(ctx, baseDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("clone base db: %w", err)
	}
	defer os.Remove(clone)
	clonePools, err := sqlite.OpenPools(clone)
	if err != nil {
		return nil, nil, fmt.Errorf("open clone: %w", err)
	}
	defer clonePools.Close()

	if err := repromoteChanged(ctx, clonePools, repoID, branch, gitSHA, candidateChangedFiles(changes)); err != nil {
		return nil, nil, fmt.Errorf("re-promote: %w", err)
	}
	drifted, err := sqlite.NewContractDriftRepo(clonePools.ReadDB).DriftedNodesInFiles(ctx, repoID, branch, changedFiles)
	if err != nil {
		return nil, nil, fmt.Errorf("drift: %w", err)
	}
	candExported, err := sqlite.NewExportedSymbolRepo(clonePools.ReadDB).ExportedSymbolsInFiles(ctx, repoID, branch, changedFiles)
	if err != nil {
		return nil, nil, fmt.Errorf("candidate exported: %w", err)
	}
	return drifted, candExported, nil
}

// exportedSymbolsAt opens a read pool on dbPath and returns the exported
// public-surface symbols over the given files. Used for the base-ref leg (over
// the pinned base clone); the after-state leg reads its exported set off the
// re-promoted clone directly in afterStateAPISignals.
func exportedSymbolsAt(ctx context.Context, dbPath, repoID, branch string, files []string) ([]ports.ExportedSymbol, error) {
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer pools.Close()
	return sqlite.NewExportedSymbolRepo(pools.ReadDB).ExportedSymbolsInFiles(ctx, repoID, branch, files)
}

// emitAPIReport writes the indented JSON api-gate report to out.
func emitAPIReport(out io.Writer, rep apiGateReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate api: encode verdict: %w", err)
	}
	return nil
}
