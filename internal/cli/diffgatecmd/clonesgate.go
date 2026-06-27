// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// CloneParams are the exact-clone diff-gate inputs. Unlike the verify gate this
// is a BLANKET gate - no target finding - so it needs only the change refs.
type CloneParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Format       string // json (default) | sarif
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
	p.Branch = resolveBranch(ctx, pools.ReadDB, p.RepoID, p.Branch)

	// Fail closed with a clean verdict when the repo isn't indexed.
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := cloneGateReport{
			CloneVerdict: diffgate.CloneVerdict{Pass: false, Checked: false},
			Failures:     []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitClones(ctx, p, pools, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (%s)", ErrGateFailed, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
	}

	// Base graph is pinned to base-ref (not the live index) so an index that has
	// advanced past base-ref cannot mask a net-new clone.
	eph, _, _, cleanup, err := buildPinnedEphemeral(ctx, ephemeralParams{
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

	verdict, err := diffgate.NewCloneGate().Evaluate(ctx, eph)
	if err != nil {
		return fmt.Errorf("diff-gate clones: evaluate: %w", err)
	}

	rep := cloneGateReport{CloneVerdict: verdict, Failures: verdict.Failures()}
	if err := emitClones(ctx, p, pools, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// emitClones writes the clone-gate result as the JSON envelope (default) or
// SARIF. SARIF best-effort resolves each member's line from the base index; a
// member ADDED by the candidate misses and falls back to its file path.
func emitClones(ctx context.Context, p CloneParams, pools *sqlite.Pools, rep cloneGateReport) error {
	if p.Format == formatSARIF {
		loc := newNodeLocator(ctx, sqlite.NewNodeLookupRepo(pools.ReadDB), p.RepoID, p.Branch, cloneNodeIDs(rep.CloneVerdict))
		return emitSarif(p.Out, cloneSarifLog(rep.CloneVerdict, loc))
	}
	return emitCloneReport(p.Out, rep)
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

// buildPinnedEphemeral is the index-ahead-safe variant of buildEphemeral
// Instead of pairing the candidate overlay with the LIVE
// index - whose SHA can drift ahead of base-ref and silently mask a net-new
// finding (the documented index-ahead false-PASS) - it pins the base graph to
// base-ref: it clones the index and re-promotes the changed files' BASE-REF
// content onto the clone, generalising discovery.go's symmetric re-promote. Both
// the ChangedNodeIDs content-hash comparison AND any base-state lookup (e.g.
// clones' NodesByContentHash) then read a base-ref-pinned graph, so the gate
// compares base-ref vs candidate regardless of how far the index has advanced.
// It returns the (base-ref-pinned) ephemeral, the candidate FileChanges, the
// base-clone DB path - gates that build a candidate after-state clone (untested,
// cycles, api) clone FROM this so their pre-state is base-ref too, not the live
// index - and a cleanup func the caller MUST defer (it closes the base-clone
// pools and removes the file; the pools stay open until then because eph.Base
// reads from them).
// Soundness: unchanged-file state is inherited from the index by BOTH this base
// clone and any candidate clone derived from it, so the index's drift on
// unchanged files cancels in every comparison; only the changed files differ
// (base-ref content here vs candidate content), which is exactly what each gate
// measures (discovery.go / ll57.10/.13).
func buildPinnedEphemeral(ctx context.Context, p ephemeralParams, dbPath string) (*diffgate.Ephemeral, []diffgate.FileChange, string, func(), error) {
	noop := func() {}
	src, err := diffgate.NewRefChangeSource(p.RepoRoot, p.BaseRef, p.CandidateRef, git.ChangedFilesBetween, fileAtRef)
	if err != nil {
		return nil, nil, "", noop, fmt.Errorf("diff-gate: change source: %w", err)
	}
	changes, err := src.Changes(ctx)
	if err != nil {
		// A bad/unknown ref is a user input error; surface a clean, ref-naming
		// message instead of raw git plumbing ( F3). Shared by every
		// clone-based gate (api/coverage/cycles/clones).
		return nil, nil, "", noop, fmt.Errorf("diff-gate: read changes: %w", cleanRefError(err, p.BaseRef, p.CandidateRef))
	}

	baseClonePath, err := cloneDB(ctx, dbPath)
	if err != nil {
		return nil, nil, "", noop, fmt.Errorf("diff-gate: clone base db: %w", err)
	}
	cleanup := func() { _ = os.Remove(baseClonePath) }
	basePools, err := sqlite.OpenPools(baseClonePath)
	if err != nil {
		cleanup()
		return nil, nil, "", noop, fmt.Errorf("diff-gate: open base clone: %w", err)
	}
	cleanup = func() { basePools.Close(); _ = os.Remove(baseClonePath) }

	// Build the base-ref file map for the changed files and re-promote it over
	// the clone, reverting those files to their base-ref state:
	//   modified file → its base-ref content
	//   deleted file (present at base-ref) → its base-ref content (base keeps it)
	//   ADDED file (absent at base-ref) → nil (DELETE from the clone). This is
	//     the crux for index-ahead: a drifted index already holds the added
	//     file, so the clone inherits it; skipping (discovery's baseChangedFiles)
	//     would leave that stale copy and the net-new clone would still cancel.
	//     Mapping to nil removes it, so the base graph truly reflects base-ref.
	//     When the index is NOT ahead the file is absent anyway, so the delete is
	//     a harmless no-op.
	readBaseContent := func(ctx context.Context, path string) ([]byte, error) {
		return adaptAbsence(git.FileAtRef(ctx, p.RepoRoot, p.BaseRef, path))
	}
	baseFiles := make(map[string][]byte, len(changes))
	for _, c := range changes {
		content, rerr := readBaseContent(ctx, c.Path)
		if rerr != nil {
			if errors.Is(rerr, diffgate.ErrFileAbsentAtRef) {
				baseFiles[c.Path] = nil // added file: absent at base-ref → delete
				continue
			}
			cleanup()
			return nil, nil, "", noop, fmt.Errorf("diff-gate: read base content %q: %w", c.Path, rerr)
		}
		baseFiles[c.Path] = content
	}
	if err := repromoteChanged(ctx, basePools, p.RepoID, p.Branch, p.BaseRef, baseFiles); err != nil {
		cleanup()
		return nil, nil, "", noop, fmt.Errorf("diff-gate: re-promote base: %w", err)
	}

	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(basePools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(basePools.ReadDB),
	}
	ix, err := diffgate.NewIndexer(treesitter.NewGoParser())
	if err != nil {
		cleanup()
		return nil, nil, "", noop, fmt.Errorf("diff-gate: indexer: %w", err)
	}
	eph, err := ix.Index(ctx, p.RepoID, p.Branch, base, src)
	if err != nil {
		cleanup()
		return nil, nil, "", noop, fmt.Errorf("diff-gate: index candidate: %w", err)
	}
	return eph, changes, baseClonePath, cleanup, nil
}

// buildEphemeral is the shared diff-gate harness: it parses the candidate
// change (base-ref.candidate-ref) into an overlay and pairs it with the
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
		return nil, nil, fmt.Errorf("diff-gate: index candidate: %w", cleanRefError(err, p.BaseRef, p.CandidateRef))
	}
	changes, err := src.Changes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("diff-gate: read changes: %w", cleanRefError(err, p.BaseRef, p.CandidateRef))
	}
	return eph, changes, nil
}
