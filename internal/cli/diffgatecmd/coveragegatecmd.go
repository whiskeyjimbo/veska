package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// UntestedParams are the diff-coverage gate inputs. Like clones it is a blanket
// gate (no target finding) and DOES need the indexed base graph (the re-promote
// clones it; the changed-node set is derived against it).
type UntestedParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// untestedReport is the JSON envelope: the verdict plus its failing-check names.
type untestedReport struct {
	diffgate.CoverageVerdict
	Failures []string `json:"failures"`
}

// RunUntested gates the candidate on changed prod symbols that no test reaches.
// It re-promotes the candidate's changed files into a throwaway clone of the
// base graph (so cross-file test→prod CALLS edges RESOLVE — the ephemeral
// overlay only carries intra-file resolved edges), runs the untested-symbol
// check (solov2-zvh6.3) over that after-state scoped to the changed files, and
// FAILs on untested findings whose node is in the node-precision changed set.
func RunUntested(ctx context.Context, p UntestedParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate untested: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate untested: open store: %w", err)
	}
	defer pools.Close()

	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved

	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := untestedReport{
			CoverageVerdict: diffgate.CoverageVerdict{Pass: false},
			Failures:        []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitUntestedReport(p.Out, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (repo_not_indexed: index %q first, e.g. `veska reindex`)", ErrGateFailed, p.RepoID)
	}

	// Pin base to base-ref (index-ahead hardening, solov2-zvh6.11): the base
	// clone re-promotes base-ref's changed files so ChangedNodeIDs content-hashes
	// the candidate against base-ref, not a drifted index.
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

	// The after-state clone derives from the base-ref-pinned clone (baseClonePath),
	// but unionCoverage's base term stays the LIVE INDEX (pools): inbound test→prod
	// CALLS edges from UNCHANGED test files are invariant to the changed prod file's
	// drift, so the live index is the sound source for the cascade-restoring splice
	// even when the index has advanced past base-ref (the base clone re-promoted the
	// changed prod files and cascade-deleted exactly those inbound edges).
	untested, err := untestedInChangedFiles(ctx, pools, baseClonePath, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
	if err != nil {
		return fmt.Errorf("diff-gate untested: %w", err)
	}

	verdict := diffgate.NewCoverageGate().Evaluate(eph.ChangedNodeIDs(ctx), untested)

	rep := untestedReport{CoverageVerdict: verdict, Failures: verdict.Failures()}
	if err := emitUntestedReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// untestedInChangedFiles re-promotes the candidate's changed files into a
// throwaway clone of the base graph and runs the untested-symbol check over the
// changed files, returning its findings (node-anchored) in-memory. The clone
// resolves cross-file test→prod CALLS so a test added in the SAME diff counts.
//
// Caller attribution is the UNION of base and clone: re-promoting a changed
// prod file delete-replaces its nodes, which CASCADE-deletes inbound CALLS
// edges from UNCHANGED caller files (the edges schema is ON DELETE CASCADE on
// dst_node_id) — so the clone alone loses the test→prod edge of a pre-existing
// test, falsely flagging a modified-but-tested symbol. The base graph still
// holds that edge; the union restores it. So after-state test callers =
// base callers (unchanged callers) ∪ clone callers (changed/added callers).
//
// It deliberately does NOT use fullCheckPass/openStructuralFindingIDs — those
// filter to structuralRules (dead-code, contract-drift), which excludes
// untested-symbol and would silently drop every finding (a false PASS). The
// check's in-memory []*domain.Finding carry the node_id anchors the gate needs.
func untestedInChangedFiles(ctx context.Context, basePools *sqlite.Pools, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, changedFiles []string) ([]*domain.Finding, error) {
	clone, err := cloneDB(ctx, baseDBPath)
	if err != nil {
		return nil, fmt.Errorf("clone base db: %w", err)
	}
	defer os.Remove(clone)
	clonePools, err := sqlite.OpenPools(clone)
	if err != nil {
		return nil, fmt.Errorf("open clone: %w", err)
	}
	defer clonePools.Close()

	if err := repromoteChanged(ctx, clonePools, repoID, branch, gitSHA, candidateChangedFiles(changes)); err != nil {
		return nil, fmt.Errorf("re-promote: %w", err)
	}

	changedSet := make(map[string]struct{}, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = struct{}{}
	}
	union := unionCoverage{
		base:    sqlite.NewCoverageRepo(basePools.ReadDB),
		clone:   sqlite.NewCoverageRepo(clonePools.ReadDB),
		changed: changedSet,
	}
	// Interface-method names over the CLONE (after-state) so a candidate-added
	// interface suppresses its dispatch-tested impls too.
	check := checks.NewUntestedSymbolCheck(union,
		checks.WithUntestedInterfaceMethods(sqlite.NewDeadCodeRepo(clonePools.ReadDB)))
	return check.Run(ctx, checks.Input{RepoID: repoID, Branch: branch, GitSHA: gitSHA, FilePaths: changedFiles})
}

// unionCoverage merges two CoverageQueriers: the candidate after-state node set
// comes from the clone (re-promoted), while each node's caller files are the
// union of clone callers and base callers. This recovers inbound test→prod
// edges that the clone's re-promote cascade-deleted (see untestedInChangedFiles).
//
// Base callers are authoritative ONLY for UNCHANGED files: a base caller file
// the diff changed/deleted is stale (the clone re-promoted it and is the
// authority), so base callers in the changed set are dropped. Without this,
// modifying a prod symbol AND deleting its test would PASS — base still shows
// the now-gone test as a caller. eph.ChangedFiles and CallerFiles are both
// repo-root-relative, so the membership check matches.
type unionCoverage struct {
	base    ports.CoverageQuerier
	clone   ports.CoverageQuerier
	changed map[string]struct{}
}

func (u unionCoverage) CandidateCallersInFiles(ctx context.Context, repoID, branch string, filePaths []string) ([]ports.NodeCallers, error) {
	cloneNodes, err := u.clone.CandidateCallersInFiles(ctx, repoID, branch, filePaths)
	if err != nil {
		return nil, err
	}
	baseNodes, err := u.base.CandidateCallersInFiles(ctx, repoID, branch, filePaths)
	if err != nil {
		return nil, err
	}
	baseByID := make(map[string][]string, len(baseNodes))
	for _, n := range baseNodes {
		baseByID[n.Node.NodeID] = n.CallerFiles
	}
	for i := range cloneNodes {
		extra, ok := baseByID[cloneNodes[i].Node.NodeID]
		if !ok {
			continue
		}
		seen := make(map[string]struct{}, len(cloneNodes[i].CallerFiles))
		for _, f := range cloneNodes[i].CallerFiles {
			seen[f] = struct{}{}
		}
		for _, f := range extra {
			if _, isChanged := u.changed[f]; isChanged {
				continue // clone is authoritative for changed caller files
			}
			if _, dup := seen[f]; !dup {
				cloneNodes[i].CallerFiles = append(cloneNodes[i].CallerFiles, f)
			}
		}
	}
	return cloneNodes, nil
}

func emitUntestedReport(out io.Writer, rep untestedReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate untested: encode verdict: %w", err)
	}
	return nil
}
