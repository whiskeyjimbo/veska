// Package diffgatecmd is the CLI invocation surface for the diff-safety gate
// (solov2-ll57.2). It wires the real indexed-HEAD graph + git ref reader into
// the diffgate composer and emits the machine-readable verdict, exiting
// non-zero on FAIL for CI gating.
//
// v1 runs with finding-discovery NOT wired (diffgate.Discovery{Ran:false}), so
// the verdict is conservatively FAIL with reason "discovery_unchecked" until
// the ephemeral finding-discovery adapter lands (solov2-ll57.4). This is the
// fail-safe by design — the gate never greenlights a change whose
// no-new-findings check did not run.
package diffgatecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// Params are the diff-gate invocation inputs. The target finding is supplied as
// flags (anchor + rule) rather than looked up from storage, keeping the v1
// command free of the findings-store surface.
type Params struct {
	RepoID       string
	Branch       string
	RepoRoot     string // working dir for git ref reads
	BaseRef      string
	CandidateRef string
	AnchorNodeID string
	Rule         string
	Out          io.Writer
}

// ErrGateFailed is returned (after the JSON verdict is written) when the gate
// verdict is FAIL, so the cobra layer exits non-zero for CI.
var ErrGateFailed = errors.New("diff-gate: FAIL")

// Run indexes the candidate change against the indexed-HEAD base graph, runs
// the verify + scope-containment gate, writes the JSON verdict to p.Out, and
// returns ErrGateFailed when the verdict is not PASS.
func Run(ctx context.Context, p Params) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" || p.AnchorNodeID == "" || p.Rule == "" {
		return errors.New("diff-gate: --repo, --base-ref, --candidate-ref, --anchor and --rule are required")
	}

	target, err := domain.NewFinding(
		domain.FindingSpec{RepoID: p.RepoID, Branch: p.Branch, Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: p.Rule, Message: "diff-gate target"},
		domain.WithNodeAnchor(p.AnchorNodeID),
	)
	if err != nil {
		return fmt.Errorf("diff-gate: build target finding: %w", err)
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate: open store: %w", err)
	}
	defer pools.Close()

	base := baseGraph{
		EdgeReaderRepo: sqlite.NewEdgeReaderRepo(pools.ReadDB),
		NodeLookupRepo: sqlite.NewNodeLookupRepo(pools.ReadDB),
	}

	src, err := diffgate.NewRefChangeSource(p.RepoRoot, p.BaseRef, p.CandidateRef, git.ChangedFilesBetween, fileAtRef)
	if err != nil {
		return fmt.Errorf("diff-gate: change source: %w", err)
	}

	ix, err := diffgate.NewIndexer(treesitter.NewGoParser())
	if err != nil {
		return fmt.Errorf("diff-gate: indexer: %w", err)
	}
	eph, err := ix.Index(ctx, p.RepoID, p.Branch, base, src)
	if err != nil {
		return fmt.Errorf("diff-gate: index candidate: %w", err)
	}

	// The guard's blast radius is computed over the BASE graph only, so its
	// staging overlay is empty (the candidate overlay belongs to the ephemeral
	// changed-node set, not the radius).
	radius, err := blastradius.NewService(base, base, staging.NewArea())
	if err != nil {
		return fmt.Errorf("diff-gate: blast-radius service: %w", err)
	}
	guard, err := diffgate.NewGuard(radius)
	if err != nil {
		return fmt.Errorf("diff-gate: guard: %w", err)
	}
	gate, err := diffgate.NewGate(diffgate.NewVerifier(), guard)
	if err != nil {
		return fmt.Errorf("diff-gate: gate: %w", err)
	}

	verdict, err := gate.Evaluate(ctx, eph, target, diffgate.Discovery{Ran: false}, blastradius.Options{})
	if err != nil {
		return fmt.Errorf("diff-gate: evaluate: %w", err)
	}

	enc := json.NewEncoder(p.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(verdict); err != nil {
		return fmt.Errorf("diff-gate: encode verdict: %w", err)
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures, ","))
	}
	return nil
}

// baseGraph composes the two sqlite read repos into one diffgate.BaseGraph.
// NodeLookupRepo also satisfies the optional content-hash capability used for
// node-precision in the changed-node set.
type baseGraph struct {
	*sqlite.EdgeReaderRepo
	*sqlite.NodeLookupRepo
}

// fileAtRef adapts git.FileAtRef's absence sentinel to diffgate's so the
// RefChangeSource recognises deleted files.
func fileAtRef(ctx context.Context, repoRoot, ref, path string) ([]byte, error) {
	return adaptAbsence(git.FileAtRef(ctx, repoRoot, ref, path))
}

// adaptAbsence maps git.ErrFileNotAtRef to diffgate.ErrFileAbsentAtRef and
// passes everything else through. Factored out so the one behavioral wrinkle in
// the wiring is unit-testable without shelling git.
func adaptAbsence(b []byte, err error) ([]byte, error) {
	if err != nil {
		if errors.Is(err, git.ErrFileNotAtRef) {
			return nil, fmt.Errorf("%w: %w", diffgate.ErrFileAbsentAtRef, err)
		}
		return nil, err
	}
	return b, nil
}
