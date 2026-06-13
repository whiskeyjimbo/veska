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
// runs the cycle gate (solov2-zvh6.6), and FAILs when a strongly-connected
// component of >=2 symbols absent at base appears in the change set.
//
// PRECONDITION (shared diff-gate contract — also assumed by clones and untested):
// the indexed graph must sit at base-ref, i.e. indexed-HEAD == base-ref. Two
// legs depend on it: the base-state edge set is read from the live index, and
// the change set comes from Ephemeral.ChangedNodeIDs, which content-hashes the
// candidate overlay against the live index (diffgate/changednodes.go). If a
// daemon has advanced the index all the way TO the candidate's content for the
// changed files (essentially post-merge), both legs collapse — base already
// holds the cycle and the change set goes empty — and a net-new cycle can FALSE-
// PASS. This is an out-of-contract index-ahead race, not the common main-drift
// (drifted main won't independently contain the PR's specific cycle). Hardening
// the whole gate family to fail closed on indexed-HEAD != base-ref is tracked as
// a follow-up; see solov2-zvh6.11 (ll57.16 is the findings-side precedent).
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

	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(pools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(pools.ReadDB),
	}

	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		rep := cycleGateReport{
			CycleVerdict: diffgate.CycleVerdict{Pass: false},
			Failures:     []string{diffgate.FailRepoNotIndexed},
		}
		if err := emitCycleReport(p.Out, rep); err != nil {
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

	after, baseEdges, info, err := cycleEdgeGraphs(ctx, pools, dbPath, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
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
//
// The base graph is read straight from the indexed base DB. The after graph
// re-promotes the candidate's changed files into a throwaway clone (so
// cross-file CALLS resolve — the overlay only carries intra-file edges) and
// SPLICES the result: re-promoting a changed file delete-replaces its nodes,
// which CASCADE-deletes inbound edges from UNCHANGED source files (edges FK is
// ON DELETE CASCADE on dst_node_id). So the clone alone is missing exactly the
// edges whose src lives in an unchanged file and whose dst lives in a changed
// file. The after graph restores them from base:
//
//	after = clone-edges ∪ base-edges(srcFile∉changed ∧ dstFile∈changed)
//
// which is complete: clone covers every src∈changed (re-derived) and every
// src∉changed∧dst∉changed (untouched by the re-promote), the splice adds back
// the cascade-deleted src∉changed∧dst∈changed. A re-added edge to a node the
// diff DELETED is harmless — that node has no outbound edge in the clone, so it
// is a sink and cannot be a >=2 cycle member.
func cycleEdgeGraphs(ctx context.Context, basePools *sqlite.Pools, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, changedFiles []string) (after, base []diffgate.DirectedEdge, info map[string]diffgate.CycleMember, err error) {
	baseRows, err := sqlite.NewCycleEdgeRepo(basePools.ReadDB).DependencyEdges(ctx, repoID, branch, diffgate.DependencyKinds)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("base edges: %w", err)
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

	for _, e := range baseRows {
		base = append(base, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
	}
	for _, e := range cloneRows {
		after = append(after, diffgate.DirectedEdge{Src: e.SrcID, Dst: e.DstID})
	}
	for _, e := range baseRows {
		_, srcChanged := changedSet[e.SrcFile]
		_, dstChanged := changedSet[e.DstFile]
		if !srcChanged && dstChanged {
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
