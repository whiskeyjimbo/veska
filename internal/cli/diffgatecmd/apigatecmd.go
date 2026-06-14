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
// and FAILs on any whose visibility flag is exported (solov2-zvh6.2).
//
// PRECONDITION (shared diff-gate contract — also assumed by clones, untested and
// cycles): the indexed graph must sit at base-ref, i.e. indexed-HEAD == base-ref.
// A node's prev_signature is taken from the clone (= live index) BEFORE the
// re-promote; if a daemon has advanced the index all the way to the candidate's
// content for the changed files, prev_signature already equals the candidate's
// signature, drift never fires, and a breaking change FALSE-PASSes. This is the
// out-of-contract index-ahead race tracked for the whole gate family by
// solov2-zvh6.11 (ll57.16 is the findings-side precedent).
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

	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(pools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(pools.ReadDB),
	}

	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := apiGateReport{
			APIVerdict: diffgate.APIVerdict{Pass: false},
			Failures:   []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitAPIReport(p.Out, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (repo_not_indexed: index %q first, e.g. `veska reindex`)", ErrGateFailed, p.RepoID)
	}

	eph, changes, err := buildEphemeral(ctx, ephemeralParams{
		RepoID:       p.RepoID,
		Branch:       p.Branch,
		RepoRoot:     p.RepoRoot,
		BaseRef:      p.BaseRef,
		CandidateRef: p.CandidateRef,
	}, base)
	if err != nil {
		return err
	}

	drifted, err := driftedInChangedFiles(ctx, dbPath, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
	if err != nil {
		return fmt.Errorf("diff-gate api: %w", err)
	}

	verdict := diffgate.NewAPIGate().Evaluate(drifted)

	rep := apiGateReport{APIVerdict: verdict, Failures: verdict.Failures()}
	if err := emitAPIReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// driftedInChangedFiles re-promotes the candidate's changed files into a
// throwaway clone of the base graph and returns the contract-drift rows over
// those files (nodes whose prev_signature != signature). The re-promote is what
// sets prev_signature (base) vs signature (candidate); the drift query is
// self-scoping to genuinely-changed signatures, so the gate needs no separate
// change-set intersection. Visibility filtering (exported-only) lives in the
// gate, leaving the whole-repo contract-drift querier unchanged.
func driftedInChangedFiles(ctx context.Context, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, changedFiles []string) ([]ports.DriftedNode, error) {
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
	return sqlite.NewContractDriftRepo(clonePools.ReadDB).DriftedNodesInFiles(ctx, repoID, branch, changedFiles)
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
