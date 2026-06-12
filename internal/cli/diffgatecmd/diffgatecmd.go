// Package diffgatecmd is the CLI invocation surface for the diff-safety gate
// (solov2-ll57.2). It wires the real indexed-HEAD graph + git ref reader into
// the diffgate composer and emits the machine-readable verdict, exiting
// non-zero on FAIL for CI gating.
//
// Structural finding-discovery (dead-code, contract-drift) is wired: the
// candidate is re-promoted into a throwaway clone of the base graph and the
// real checks run over the whole graph, so a change that introduces a new
// structural finding FAILs. Any discovery error degrades to Ran=false, so the
// gate FAILs with "discovery_unchecked" rather than risking a false green.
// Line/dep scanners (secrets, vuln) are out of v1 discovery scope.
package diffgatecmd

import (
	"context"
	"database/sql"
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

	// Fail closed with a clean verdict when the repo isn't indexed (missing
	// schema or zero nodes), rather than letting a downstream "no such table"
	// crash escape with no JSON. CI consumers always get a parseable verdict
	// and a non-zero exit.
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		v := diffgate.GateVerdict{Pass: false, Failures: []string{diffgate.FailRepoNotIndexed}}
		if err := emitVerdict(p.Out, v); err != nil {
			return err
		}
		return fmt.Errorf("%w (repo_not_indexed: index %q first, e.g. `veska reindex`)", ErrGateFailed, p.RepoID)
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

	// Structural finding-discovery over the candidate (degrades to Ran=false on
	// any error → gate FAILs discovery_unchecked, never a false green).
	changes, err := src.Changes(ctx)
	if err != nil {
		return fmt.Errorf("diff-gate: read changes: %w", err)
	}
	disc := discover(ctx, dbPath, p, changes)

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

	verdict, err := gate.Evaluate(ctx, eph, target, disc, blastradius.Options{})
	if err != nil {
		return fmt.Errorf("diff-gate: evaluate: %w", err)
	}

	if err := emitVerdict(p.Out, verdict); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures, ","))
	}
	return nil
}

// emitVerdict writes the verdict as indented JSON. Every non-PASS path goes
// through here so a CI consumer always gets a parseable verdict on stdout.
func emitVerdict(out io.Writer, v diffgate.GateVerdict) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("diff-gate: encode verdict: %w", err)
	}
	return nil
}

// repoIndexed reports whether (repoID, branch) has any indexed nodes. A missing
// schema (fresh/empty db → "no such table: nodes") counts as not indexed.
func repoIndexed(ctx context.Context, db *sql.DB, repoID, branch string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE repo_id=? AND branch=?`, repoID, branch).Scan(&n)
	return err == nil && n > 0
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
