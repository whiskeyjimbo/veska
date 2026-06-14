package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// CloneParams are the exact-clone diff-gate inputs. Unlike the verify gate this
// is a BLANKET gate — no target finding — so it needs only the change refs.
type CloneParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// cloneGateReport is the JSON envelope: the verdict plus the named failing
// checks, so CI/agents read one machine-parseable object.
type cloneGateReport struct {
	diffgate.CloneVerdict
	Failures []string `json:"failures"`
}

// RunClones indexes the candidate against the indexed-HEAD base graph and runs
// the exact-clone gate, emitting the JSON verdict and returning ErrGateFailed
// when net-new duplication is found (or the gate could not be checked).
func RunClones(ctx context.Context, p CloneParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate clones: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate clones: open store: %w", err)
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

	// Fail closed with a clean verdict when the repo isn't indexed.
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := cloneGateReport{
			CloneVerdict: diffgate.CloneVerdict{Pass: false, Checked: false},
			Failures:     []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitCloneReport(p.Out, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (repo_not_indexed: index %q first, e.g. `veska reindex`)", ErrGateFailed, p.RepoID)
	}

	eph, _, err := buildEphemeral(ctx, ephemeralParams{
		RepoID:       p.RepoID,
		Branch:       p.Branch,
		RepoRoot:     p.RepoRoot,
		BaseRef:      p.BaseRef,
		CandidateRef: p.CandidateRef,
	}, base)
	if err != nil {
		return err
	}

	verdict, err := diffgate.NewCloneGate().Evaluate(ctx, eph)
	if err != nil {
		return fmt.Errorf("diff-gate clones: evaluate: %w", err)
	}

	rep := cloneGateReport{CloneVerdict: verdict, Failures: verdict.Failures()}
	if err := emitCloneReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// emitCloneReport writes the indented JSON clone-gate report to out.
func emitCloneReport(out io.Writer, rep cloneGateReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate clones: encode verdict: %w", err)
	}
	return nil
}

// ephemeralParams carries the change-source inputs buildEphemeral needs. It is
// the common shape every diff-twin gate (clones now; net-new findings, cycles,
// breaking-API next) feeds in to obtain the (base, candidate) substrate.
type ephemeralParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
}

// buildEphemeral is the shared diff-gate harness: it parses the candidate
// change (base-ref..candidate-ref) into an overlay and pairs it with the
// supplied base graph, producing the ephemeral (base, candidate) substrate the
// gates query. It also returns the candidate's raw FileChanges, which gates
// that re-promote the candidate (the untested gate) need for file content;
// gates that only query the overlay (clones) ignore them. Extracted so every
// diff-twin gate constructs the substrate the same way (the verify gate
// predates it and keeps its own inline wiring).
func buildEphemeral(ctx context.Context, p ephemeralParams, base diffgate.BaseGraph) (*diffgate.Ephemeral, []diffgate.FileChange, error) {
	src, err := diffgate.NewRefChangeSource(p.RepoRoot, p.BaseRef, p.CandidateRef, git.ChangedFilesBetween, fileAtRef)
	if err != nil {
		return nil, nil, fmt.Errorf("diff-gate: change source: %w", err)
	}
	ix, err := diffgate.NewIndexer(treesitter.NewGoParser())
	if err != nil {
		return nil, nil, fmt.Errorf("diff-gate: indexer: %w", err)
	}
	eph, err := ix.Index(ctx, p.RepoID, p.Branch, base, src)
	if err != nil {
		return nil, nil, fmt.Errorf("diff-gate: index candidate: %w", err)
	}
	changes, err := src.Changes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("diff-gate: read changes: %w", err)
	}
	return eph, changes, nil
}
