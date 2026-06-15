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
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
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
	// FindingID, when set, identifies the target finding by its id (the first
	// column of `veska findings list`); its anchor + rule are derived from the
	// stored row. Otherwise AnchorNodeID + Rule are used directly.
	FindingID    string
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
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return errors.New("diff-gate: --repo, --base-ref and --candidate-ref are required")
	}
	// The target finding is identified EITHER by --finding (the finding_id from
	// `veska findings list`, the ergonomic front door) OR by the explicit
	// --anchor + --rule pair (for power users / CI without a finding id).
	if p.FindingID == "" && (p.AnchorNodeID == "" || p.Rule == "") {
		return errors.New("diff-gate: identify the target with --finding <id>, or with both --anchor and --rule")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate: open store: %w", err)
	}
	defer pools.Close()

	// Resolve --repo as a full id, a 12-char short id (what `repo add` and
	// `veska findings list` print), or an unambiguous hex prefix → the canonical
	// full repo_id every downstream query keys on. Without this a junior who
	// pastes the short id gets a spurious repo_not_indexed against an indexed repo
	// (solov2-ll57.15). Unknown ids pass through and fail closed below.
	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved

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
		return fmt.Errorf("%w (%s)", ErrGateFailed, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
	}

	target, err := resolveTarget(ctx, pools.ReadDB, p)
	if err != nil {
		return err
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
		return fmt.Errorf("diff-gate: index candidate: %w", cleanRefError(err, p.BaseRef, p.CandidateRef))
	}

	// Structural finding-discovery over the candidate (degrades to Ran=false on
	// any error → gate FAILs discovery_unchecked, never a false green).
	changes, err := src.Changes(ctx)
	if err != nil {
		return fmt.Errorf("diff-gate: read changes: %w", cleanRefError(err, p.BaseRef, p.CandidateRef))
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

// resolveTarget builds the target finding from either --finding (deriving the
// anchor + rule from the stored finding row) or the explicit --anchor + --rule.
func resolveTarget(ctx context.Context, db *sql.DB, p Params) (*domain.Finding, error) {
	anchor, rule := p.AnchorNodeID, p.Rule
	if p.FindingID != "" {
		a, r, err := lookupFinding(ctx, db, p.RepoID, p.Branch, p.FindingID)
		if err != nil {
			return nil, err
		}
		anchor, rule = a, r
	}
	target, err := domain.NewFinding(
		domain.FindingSpec{RepoID: p.RepoID, Branch: p.Branch, Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: rule, Message: "diff-gate target"},
		domain.WithNodeAnchor(anchor),
	)
	if err != nil {
		return nil, fmt.Errorf("diff-gate: build target finding: %w", err)
	}
	return target, nil
}

// lookupFinding resolves a finding_id to its (node anchor, rule). A file-anchored
// finding (NULL node_id) is rejected: the gate needs a node anchor.
func lookupFinding(ctx context.Context, db *sql.DB, repoID, branch, findingID string) (anchor, rule string, err error) {
	var node sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT node_id, rule FROM findings WHERE finding_id=? AND repo_id=? AND branch=?`,
		findingID, repoID, branch).Scan(&node, &rule)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("diff-gate: finding %q not found in repo %q branch %q", findingID, repoID, branch)
	}
	if err != nil {
		return "", "", fmt.Errorf("diff-gate: look up finding %q: %w", findingID, err)
	}
	if !node.Valid || node.String == "" {
		return "", "", fmt.Errorf("diff-gate: finding %q is file-anchored; the gate needs a node-anchored finding (pass --anchor/--rule instead)", findingID)
	}
	return node.String, rule, nil
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

// resolveRepoID maps the --repo flag to the canonical full repo_id: an exact id
// passes through; otherwise it matches a 12-char short id or an unambiguous hex
// prefix against the repos table (mirroring the MCP/registry resolver). An
// unknown id is returned unchanged so the repoIndexed fail-closed path emits a
// clean repo_not_indexed rather than a confusing match error. A missing repos
// table (fresh/empty db) also passes through unchanged.
func resolveRepoID(ctx context.Context, db *sql.DB, repoID string) (string, error) {
	var exact string
	switch err := db.QueryRowContext(ctx, `SELECT repo_id FROM repos WHERE repo_id = ?`, repoID).Scan(&exact); {
	case err == nil:
		return exact, nil
	case errors.Is(err, sql.ErrNoRows):
		// fall through to short-id / prefix matching
	default:
		return repoID, nil // repos table unavailable — never worse than no resolution
	}

	rows, err := db.QueryContext(ctx, `SELECT repo_id FROM repos`)
	if err != nil {
		return repoID, nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("diff-gate: resolve repo %q: %w", repoID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("diff-gate: resolve repo %q: %w", repoID, err)
	}
	for _, id := range ids {
		if repocmd.ShortRepoID(id) == repoID {
			return id, nil
		}
	}
	if len(repoID) >= 4 {
		var matched string
		for _, id := range ids {
			if strings.HasPrefix(id, repoID) {
				if matched != "" {
					return "", fmt.Errorf("diff-gate: --repo %q is ambiguous (matches multiple repos); use the full id from `veska repo list`", repoID)
				}
				matched = id
			}
		}
		if matched != "" {
			return matched, nil
		}
	}
	return repoID, nil // unknown — repoIndexed() will fail closed with repo_not_indexed
}

// repoIndexed reports whether (repoID, branch) has any indexed nodes. A missing
// schema (fresh/empty db → "no such table: nodes") counts as not indexed.
func repoIndexed(ctx context.Context, db *sql.DB, repoID, branch string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE repo_id=? AND branch=?`, repoID, branch).Scan(&n)
	return err == nil && n > 0
}

// repoKnown reports whether repoID is a registered repo. It distinguishes an
// UNKNOWN --repo handle (a name/typo that resolveRepoID could not match to any
// id) from a registered-but-UN-indexed repo — the two conditions a junior
// conflates when an indexed repo, addressed by its name, reports "not indexed"
// (solov2-i0tx.2 F2). A missing repos table counts as "not known".
func repoKnown(ctx context.Context, db *sql.DB, repoID string) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos WHERE repo_id=?`, repoID).Scan(&n)
	return err == nil && n > 0
}

// notIndexedDetail renders the actionable reason a (repoID, branch) is not
// usable: an unknown handle points the junior back at `veska repo list`; a
// registered-but-unindexed repo points at `veska reindex`. Shared by every
// diff-gate subcommand's not-indexed branch so the distinction is uniform.
func notIndexedDetail(ctx context.Context, db *sql.DB, repoID string) string {
	if !repoKnown(ctx, db, repoID) {
		return fmt.Sprintf("unknown repo %q — use the REPO_ID from `veska repo list`", repoID)
	}
	return fmt.Sprintf("repo_not_indexed: index %q first, e.g. `veska reindex`", repoID)
}

// cleanRefError maps git's raw "unknown revision" plumbing to an actionable
// message naming the offending refs (solov2-i0tx.2 F3). The typed
// git.ErrUnknownRevision exists precisely so callers need not string-match.
// Non-ref errors pass through unchanged.
func cleanRefError(err error, baseRef, candidateRef string) error {
	if errors.Is(err, git.ErrUnknownRevision) {
		return fmt.Errorf("ref not found (base %q .. candidate %q) — is the base committed, or is this a shallow clone?", baseRef, candidateRef)
	}
	return err
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
