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
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// CycleParams are the dependency-cycle diff-gate inputs. Like clones/untested it
// is a blanket gate (no target finding) and DOES need the indexed base graph:
// the re-promote clones it, and the net-new judgement compares the candidate's
// dependency graph against the base's.
type CycleParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// cycleGateReport is the JSON envelope: the verdict plus its failing-check names.
type cycleGateReport struct {
	diffgate.CycleVerdict
	Failures []string `json:"failures"`
}

// RunCycles gates the candidate on a net-new dependency cycle. It re-promotes the
// candidate's changed files into a throwaway clone of the base graph (so
// cross-file CALLS RESOLVE — the ephemeral overlay carries only intra-file
// edges), materialises the after- and base-state directed dependency graphs,
// runs the cycle gate, and FAILs when a strongly-connected
// component of >=2 symbols absent at base appears in the change set.
// Index-ahead safety: both the base-state edge set AND the
// candidate after-state are pinned to base-ref via buildPinnedEphemeral's base
// clone (the after-state clone chains FROM that base clone, not the live index).
// So even if a daemon has advanced the index past base-ref — all the way to the
// candidate's content — neither leg collapses: the base clone re-promotes
// base-ref's changed-file content (deleting any added file the drifted index
// carried), and ChangedNodeIDs content-hashes against that base-ref-pinned base.
// A net-new cycle therefore still FAILs regardless of index drift.
func RunCycles(ctx context.Context, p CycleParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate cycles: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate cycles: open store: %w", err)
	}
	defer pools.Close()

	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved

	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := cycleGateReport{
			CycleVerdict: diffgate.CycleVerdict{Pass: false},
			Failures:     []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitCycleReport(p.Out, rep); err != nil {
			return err
		}
		return fmt.Errorf("%w (%s)", ErrGateFailed, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
	}

	// Pin base + candidate after-state to base-ref (index-ahead hardening,
	// ). The base clone re-promotes base-ref's changed files; the
	// after-state clone below chains FROM baseClonePath, not the live index.
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

	// Read base edges and clone the after-state from the base-ref-pinned clone
	// (not the live index) so both legs reflect base-ref.
	basePools, err := sqlite.OpenPools(baseClonePath)
	if err != nil {
		return fmt.Errorf("diff-gate cycles: open base clone: %w", err)
	}
	defer basePools.Close()

	after, baseEdges, info, err := cycleEdgeGraphs(ctx, basePools, baseClonePath, pools, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
	if err != nil {
		return fmt.Errorf("diff-gate cycles: %w", err)
	}

	verdict := diffgate.NewCycleGate().Evaluate(after, baseEdges, eph.ChangedNodeIDs(ctx), info)

	rep := cycleGateReport{CycleVerdict: verdict, Failures: verdict.Failures()}
	if err := emitCycleReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// cycleEdgeGraphs materialises the after- and base-state dependency graphs for
// the cycle gate, plus a node_id->member naming map for the verdict.
// Both graphs are built from a clone the changed files are re-promoted into (so
// cross-file CALLS resolve — the ephemeral overlay only carries intra-file
// edges): the BASE clone (base-ref content, from buildPinnedEphemeral) and an
// AFTER clone derived from it (candidate content). Re-promoting a changed file
// delete-replaces its nodes, which CASCADE-deletes inbound edges from UNCHANGED
// source files (edges FK is ON DELETE CASCADE on dst_node_id). So BOTH clones
// are missing exactly the edges whose src lives in an unchanged file and whose
// dst lives in a changed file. We splice those back from the LIVE index:
//
//	base = base-clone-edges ∪ index-edges(srcFile∉changed ∧ dstFile∈changed)
//	after = after-clone-edges ∪ index-edges(srcFile∉changed ∧ dstFile∈changed)
//
// The live index is a SOUND source for that splice term under the common
// index-ahead case (body-level drift of the changed file): a cross-file inbound
// edge (src in an unchanged file) is invariant to body drift — A→B exists
// because a.go calls B and B's node_id is stable, independent of b.go's body
// so the index always holds it. (Edge case, already out of contract: if the
// drifted candidate-content index RENAMED or REMOVED the dst symbol, its
// node_id changes and the base-ref edge is absent from the index; the base-leg
// splice would then under-restore. Not chased — it is a corner of an already
// out-of-contract index-ahead race.) The drift lives only
// in edges whose src is in a changed file, and those come from the clones, not
// the splice. A re-added edge to a
// node the diff DELETED is harmless — that node has no outbound edge in the
// clone, so it is a sink and cannot be a >=2 cycle member.
//
//nolint:revive,cyclop // argument-limit + cyclomatic complexity predate zvh6.11; this change only adds the idxPools splice source and merged splice loop. The args are distinct plumbing (two clones + the live index) that don't form a cohesive struct.
func cycleEdgeGraphs(ctx context.Context, basePools *sqlite.Pools, baseDBPath string, idxPools *sqlite.Pools, repoID, branch, gitSHA string, changes []diffgate.FileChange, changedFiles []string) (after, base []diffgate.DirectedEdge, info map[string]diffgate.CycleMember, err error) {
	baseRows, err := sqlite.NewCycleEdgeRepo(basePools.ReadDB).DependencyEdges(ctx, repoID, branch, diffgate.DependencyKinds)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("base edges: %w", err)
	}
	idxRows, err := sqlite.NewCycleEdgeRepo(idxPools.ReadDB).DependencyEdges(ctx, repoID, branch, diffgate.DependencyKinds)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("index edges: %w", err)
	}

	clone, err := cloneDB(ctx, baseDBPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("clone base db: %w", err)
	}
	defer os.Remove(clone)
	clonePools, err := sqlite.OpenPools(clone)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open clone: %w", err)
	}
	defer clonePools.Close()
	if err := repromoteChanged(ctx, clonePools, repoID, branch, gitSHA, candidateChangedFiles(changes)); err != nil {
		return nil, nil, nil, fmt.Errorf("re-promote: %w", err)
	}
	cloneRows, err := sqlite.NewCycleEdgeRepo(clonePools.ReadDB).DependencyEdges(ctx, repoID, branch, diffgate.DependencyKinds)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("clone edges: %w", err)
	}

	changedSet := make(map[string]struct{}, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = struct{}{}
	}

	info = make(map[string]diffgate.CycleMember)
	record := func(rows []sqlite.DependencyEdge) {
		for _, e := range rows {
			if _, ok := info[e.SrcID]; !ok {
				info[e.SrcID] = diffgate.CycleMember{NodeID: e.SrcID, FilePath: e.SrcFile, SymbolPath: e.SrcSymbol}
			}
			if _, ok := info[e.DstID]; !ok {
				info[e.DstID] = diffgate.CycleMember{NodeID: e.DstID, FilePath: e.DstFile, SymbolPath: e.DstSymbol}
			}
		}
	}
	record(baseRows)
	record(cloneRows)
	record(idxRows) // spliced endpoints (e.g. A) get names even after their edge cascaded away

	for _, e := range baseRows {
		base = append(base, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
	}
	for _, e := range cloneRows {
		after = append(after, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
	}
	// Splice the cascade-deleted cross-file inbound edges back into BOTH legs
	// from the live index (sound under index drift; see doc comment).
	for _, e := range idxRows {
		_, srcChanged := changedSet[e.SrcFile]
		_, dstChanged := changedSet[e.DstFile]
		if !srcChanged && dstChanged {
			base = append(base, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
			after = append(after, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
		}
	}
	return after, base, info, nil
}

// emitCycleReport writes the indented JSON cycle-gate report to out.
func emitCycleReport(out io.Writer, rep cycleGateReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate cycles: encode verdict: %w", err)
	}
	return nil
}
