package diffgatecmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// structuralRules are the rules discovery soundly covers: a re-promote + a
// FULL-file check pass makes these graph-native checks correct over the
// candidate. Line/dep scanners (secrets, vuln) need per-line/dep inputs and are
// out of v1 scope; they are deliberately NOT registered, and the finding diff
// is filtered to these rules so a base-promotion finding from another rule
// can't leak into the verdict.
var structuralRules = []string{"dead-code", "contract-drift"}

// discoverActor records discovery's ephemeral re-promotions in the throwaway
// clone. Never touches the real graph.
var discoverActor = domain.Actor{ID: "service:veska-diff-gate", Kind: domain.ActorKindSystem}

// DiscoverStructural produces the base/candidate structural finding-id sets for
// the gate's no-new-findings check. It is SOUND by construction:
//
//   - Base findings come from running the real dead-code + contract-drift
//     checks over the WHOLE existing (cloned) graph — its edges are already
//     correctly resolved, so no re-promote is needed.
//   - Candidate findings come from re-promoting the COMPLETE candidate file set
//     (every file, changed content overlaid on base) into the clone, then
//     running the same checks over the whole graph. The batch is complete, so
//     the promoter's batch-scoped call resolution (buildPackageSymbolMap /
//     buildModuleRelSymbolMap operate over p.batch only) rebinds every
//     cross-file call correctly — a PARTIAL re-promote would drop a changed
//     file's calls into unchanged files and spuriously flag their callees dead.
//
// Running checks over ALL files (not just changed) on each side is what catches
// a node that became dead in an UNCHANGED file because the change removed its
// last caller — the false-green a per-file check scope would miss.
//
// changes are the candidate's changed files (new content, or Deleted). The
// complete candidate file set is assembled internally: every file_path in the
// base graph, overlaid with the changes — unchanged files are read through
// readUnchanged (in the live path, git.FileAtRef at the base ref; their content
// is unchanged, so the base ref is authoritative). It mutates only the
// throwaway clone (removed on return) and performs no network IO beyond
// readUnchanged.
func DiscoverStructural(ctx context.Context, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, readUnchanged func(ctx context.Context, path string) ([]byte, error)) (diffgate.Discovery, error) {
	clone, err := cloneDB(ctx, baseDBPath)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: clone base db: %w", err)
	}
	defer os.Remove(clone)

	pools, err := sqlite.OpenPools(clone)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: open clone: %w", err)
	}
	defer pools.Close()

	runner := buildStructuralRunner(pools)

	// Base findings: the clone's graph already has correctly-resolved edges.
	baseIDs, err := fullCheckPass(ctx, pools, runner, repoID, branch, gitSHA)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: base pass: %w", err)
	}

	// Assemble the COMPLETE candidate file set from the base graph's file list
	// overlaid with the changes — a complete batch is what makes the promoter's
	// batch-scoped resolution rebind all cross-file calls.
	baseFiles, err := allFiles(ctx, pools.ReadDB, repoID, branch)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: list base files: %w", err)
	}
	candidateFiles, err := assembleCandidate(ctx, baseFiles, changes, readUnchanged)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: assemble candidate files: %w", err)
	}

	if err := repromoteAll(ctx, pools, repoID, branch, gitSHA, candidateFiles); err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: re-promote candidate: %w", err)
	}

	candIDs, err := fullCheckPass(ctx, pools, runner, repoID, branch, gitSHA)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: candidate pass: %w", err)
	}

	return diffgate.Discovery{
		Ran:          true,
		BaseIDs:      baseIDs,
		CandidateIDs: candIDs,
		CoveredRules: append([]string(nil), structuralRules...),
	}, nil
}

// assembleCandidate builds the complete candidate file set: every base file,
// overlaid with the changes. A changed file uses its new content (nil when
// deleted, so promotion drops its nodes); an added file (not in the base graph)
// is appended; an unchanged base file is read through readUnchanged.
func assembleCandidate(ctx context.Context, baseFiles []string, changes []diffgate.FileChange, readUnchanged func(ctx context.Context, path string) ([]byte, error)) (map[string][]byte, error) {
	changed := make(map[string]diffgate.FileChange, len(changes))
	for _, c := range changes {
		changed[c.Path] = c
	}
	out := make(map[string][]byte, len(baseFiles)+len(changes))
	seen := make(map[string]struct{}, len(baseFiles))
	for _, f := range baseFiles {
		seen[f] = struct{}{}
		if c, ok := changed[f]; ok {
			out[f] = c.Content // nil when deleted → promotion drops the file's nodes
			continue
		}
		content, err := readUnchanged(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("read unchanged file %q: %w", f, err)
		}
		out[f] = content
	}
	// Added files: present in the change set but not in the base graph.
	for _, c := range changes {
		if _, ok := seen[c.Path]; ok || c.Deleted {
			continue
		}
		out[c.Path] = c.Content
	}
	return out, nil
}

// discover runs structural finding-discovery for the live gate. ANY error
// degrades to a not-run Discovery (Ran=false) so the gate FAILs with
// discovery_unchecked rather than risking a false green — the fail-safe. The
// unchanged-file reader is git.FileAtRef at the base ref (an unchanged file's
// content is identical at the base ref).
func discover(ctx context.Context, dbPath string, p Params, changes []diffgate.FileChange) diffgate.Discovery {
	readUnchanged := func(ctx context.Context, path string) ([]byte, error) {
		return adaptAbsence(git.FileAtRef(ctx, p.RepoRoot, p.BaseRef, path))
	}
	disc, err := DiscoverStructural(ctx, dbPath, p.RepoID, p.Branch, p.CandidateRef, changes, readUnchanged)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diff-gate: discovery degraded (no-new-findings unchecked): %v\n", err)
		return diffgate.Discovery{Ran: false}
	}
	return disc
}

// cloneDB copies the committed contents of the base DB into a fresh temp file
// via VACUUM INTO (a consistent snapshot of committed state, WAL included). The
// caller removes the returned path.
func cloneDB(ctx context.Context, src string) (string, error) {
	f, err := os.CreateTemp("", "diffgate-clone-*.db")
	if err != nil {
		return "", err
	}
	dst := f.Name()
	_ = f.Close()
	_ = os.Remove(dst) // VACUUM INTO requires the target not to exist.

	db, err := sql.Open(sqldriver.Name, sqldriver.BuildDSN(src, 5000))
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// buildStructuralRunner registers the graph-structural checks against the clone
// and returns a runner that persists their findings into it. Secrets/vuln are
// intentionally omitted (out of v1 discovery scope).
func buildStructuralRunner(pools *sqlite.Pools) *checks.Runner {
	reg := checks.NewRegistry()
	repoKind := func(ctx context.Context, id string) (string, error) {
		rec, err := repo.Get(ctx, pools.ReadDB, id)
		if err != nil {
			// Fail open as a tracked repo so dead-code still runs (mirrors the
			// daemon's fail-open on a registry hiccup).
			return "tracked", nil
		}
		return rec.Kind, nil
	}
	reg.Register(checks.NewDeadCodeCheck(
		sqlite.NewDeadCodeRepo(pools.ReadDB),
		checks.WithDeadCodeRepoKindLookup(repoKind),
	))
	reg.Register(checks.NewContractDriftCheck(sqlite.NewContractDriftRepo(pools.ReadDB)))
	return checks.NewRunner(reg, sqlite.NewFindingRepo(pools.Write), nil)
}

// fullCheckPass runs the structural checks over EVERY file in (repoID, branch)
// — making absence-triggered rules like dead-code sound across files the change
// did not touch directly — then reads back the open structural finding ids.
func fullCheckPass(ctx context.Context, pools *sqlite.Pools, runner *checks.Runner, repoID, branch, gitSHA string) ([]string, error) {
	files, err := allFiles(ctx, pools.ReadDB, repoID, branch)
	if err != nil {
		return nil, err
	}
	runner.Run(ctx, checks.Input{RepoID: repoID, Branch: branch, GitSHA: gitSHA, FilePaths: files})
	return openStructuralFindingIDs(ctx, pools.ReadDB, repoID, branch)
}

// repromoteAll stages the COMPLETE candidate file set and promotes it as one
// batch through the real Promoter. The batch being complete is what makes the
// promoter's batch-scoped call resolution rebind every cross-file call. A file
// mapped to nil content stages no nodes (a deletion). Checks are NOT run during
// promotion — fullCheckPass runs them over the whole graph afterwards.
func repromoteAll(ctx context.Context, pools *sqlite.Pools, repoID, branch, gitSHA string, candidateFiles map[string][]byte) error {
	core := composition.NewColdScanCore(pools, nil, nil)
	for path, content := range candidateFiles {
		core.Ingester.SaveColdScan(ctx, repoID, branch, path, content)
	}
	return core.Promoter.Promote(ctx, repoID, branch, gitSHA, discoverActor)
}

func allFiles(ctx context.Context, db *sql.DB, repoID, branch string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT file_path FROM nodes WHERE repo_id=? AND branch=?`, repoID, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// openStructuralFindingIDs reads the open finding ids for the structural rules
// only — a base-promotion finding from another rule (parse-failure, auto-link)
// must not leak into the diff, since discovery doesn't re-evaluate those.
func openStructuralFindingIDs(ctx context.Context, db *sql.DB, repoID, branch string) ([]string, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(structuralRules)), ",")
	query := fmt.Sprintf(`SELECT finding_id FROM findings
	          WHERE repo_id=? AND branch=? AND state='open' AND rule IN (%s)`, placeholders)
	args := []any{repoID, branch}
	for _, r := range structuralRules {
		args = append(args, r)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
