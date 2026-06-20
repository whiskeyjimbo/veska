// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"database/sql"
	"errors"
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
//	Each side clones the base graph, re-promotes ONLY the CHANGED files over
//	  that clone (base content for the base side, candidate content for the
//	  candidate side), then runs the real dead-code + contract-drift checks
//	  across the WHOLE graph.
//	Re-promoting only the changed files is sound because the promoter resolves
//	  a changed file's calls into UNCHANGED same-package siblings against the
//	  already-promoted graph, and the inbound edge that makes a
//	  symbol in an unchanged file newly-dead is OWNED by the changed file - so
//	  deleting/rebinding the changed file's edges is enough to surface it.
//	Running the check pass over ALL files (not just the changed ones) is what
//	  catches a node that became dead in an UNCHANGED file because the change
//	  removed its last caller - the false-green a per-file check scope misses.
//
// Both sides run the IDENTICAL clone + re-promote-changed + check path, so an
// inert change yields no spurious "new" finding from a graph-build difference
// It mutates only the throwaway clones (removed on return) and
// reads only the changed files' base content via readBaseContent - O(changed),
// not O(repo), the whole point of.
// changes are the candidate's changed files (new content, or Deleted).
// readBaseContent supplies a changed file's content at the base ref (in the live
// path, git.FileAtRef at the base ref); it is consulted only for changed paths.
func DiscoverStructural(ctx context.Context, baseDBPath, repoID, branch, gitSHA string, changes []diffgate.FileChange, readBaseContent func(ctx context.Context, path string) ([]byte, error)) (diffgate.Discovery, error) {
	candFiles := candidateChangedFiles(changes)
	baseFiles, err := baseChangedFiles(ctx, changes, readBaseContent)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: read base content: %w", err)
	}

	baseIDs, err := findingsForChangedFiles(ctx, baseDBPath, repoID, branch, gitSHA, baseFiles)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: base findings: %w", err)
	}
	candIDs, err := findingsForChangedFiles(ctx, baseDBPath, repoID, branch, gitSHA, candFiles)
	if err != nil {
		return diffgate.Discovery{}, fmt.Errorf("diff-gate discovery: candidate findings: %w", err)
	}

	return diffgate.Discovery{
		Ran:          true,
		BaseIDs:      baseIDs,
		CandidateIDs: candIDs,
		CoveredRules: append([]string(nil), structuralRules...),
	}, nil
}

// candidateChangedFiles maps each changed path to its candidate content - nil
// for a deletion, so re-promotion drops the file's nodes. Added files (not in
// the base graph) are included; the promoter inserts their nodes.
func candidateChangedFiles(changes []diffgate.FileChange) map[string][]byte {
	out := make(map[string][]byte, len(changes))
	for _, c := range changes {
		if c.Deleted {
			out[c.Path] = nil
			continue
		}
		out[c.Path] = c.Content
	}
	return out
}

// baseChangedFiles maps each changed path to its content at the BASE ref. An
// ADDED file (absent at base → ErrFileAbsentAtRef) is skipped: it never existed
// in the base graph, so the base side must not promote it. The result is what
// the base side re-promotes - a no-op over the clone (same content already
// promoted) that keeps the build path symmetric with the candidate side.
func baseChangedFiles(ctx context.Context, changes []diffgate.FileChange, readBaseContent func(ctx context.Context, path string) ([]byte, error)) (map[string][]byte, error) {
	out := make(map[string][]byte, len(changes))
	for _, c := range changes {
		content, err := readBaseContent(ctx, c.Path)
		if err != nil {
			if errors.Is(err, diffgate.ErrFileAbsentAtRef) {
				continue // added file: not present at base
			}
			return nil, fmt.Errorf("read base content %q: %w", c.Path, err)
		}
		out[c.Path] = content
	}
	return out, nil
}

// findingsForChangedFiles computes the open structural finding ids after
// re-promoting ONLY the changed files, built identically for both the base and
// candidate sides: clone the base db (for its repos registration + module
// metadata, which the promoter's cross-package resolution needs), re-promote the
// changed files over it, and run the checks across the whole graph. Both sides
// go through this same path, so only genuine content differences survive the
// base-vs-candidate diff.
func findingsForChangedFiles(ctx context.Context, baseDBPath, repoID, branch, gitSHA string, files map[string][]byte) ([]string, error) {
	clone, err := cloneDB(ctx, baseDBPath)
	if err != nil {
		return nil, fmt.Errorf("clone base db: %w", err)
	}
	defer os.Remove(clone)
	pools, err := sqlite.OpenPools(clone)
	if err != nil {
		return nil, fmt.Errorf("open clone: %w", err)
	}
	defer pools.Close()
	// The clone inherits the indexed graph's findings table. dead-code and
	// contract-drift are NOT authoritative checks (only vulnscan is), so the
	// re-check below never closes a stale open finding - it would leak into this
	// side's set and, being symmetric in id, cancel a genuinely-new candidate
	// finding (a false GREEN when the index sits AHEAD of base).
	// Clear the structural findings so each side derives them purely from graph
	// state, independent of the index's ref position.
	if err := clearStructuralFindings(ctx, pools.Write, repoID, branch); err != nil {
		return nil, fmt.Errorf("clear inherited findings: %w", err)
	}
	if err := repromoteChanged(ctx, pools, repoID, branch, gitSHA, files); err != nil {
		return nil, fmt.Errorf("re-promote: %w", err)
	}
	return fullCheckPass(ctx, pools, buildStructuralRunner(pools), repoID, branch, gitSHA)
}

// clearStructuralFindings deletes the structural-rule findings inherited by the
// throwaway clone so discovery's per-side check pass starts from a clean slate.
// Safe because the clone is discarded and the structural checks read graph state
// (nodes/edges), not the findings table.
func clearStructuralFindings(ctx context.Context, db *sql.DB, repoID, branch string) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(structuralRules)), ",")
	args := []any{repoID, branch}
	for _, r := range structuralRules {
		args = append(args, r)
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM findings WHERE repo_id=? AND branch=? AND rule IN (%s)`, placeholders), args...)
	return err
}

// discover runs structural finding-discovery for the live gate. ANY error
// degrades to a not-run Discovery (Ran=false) so the gate FAILs with
// discovery_unchecked rather than risking a false green - the fail-safe. The
// base-content reader is git.FileAtRef at the base ref, consulted only for the
// changed files' base content.
func discover(ctx context.Context, dbPath string, p Params, changes []diffgate.FileChange) diffgate.Discovery {
	readBaseContent := func(ctx context.Context, path string) ([]byte, error) {
		return adaptAbsence(git.FileAtRef(ctx, p.RepoRoot, p.BaseRef, path))
	}
	disc, err := DiscoverStructural(ctx, dbPath, p.RepoID, p.Branch, p.CandidateRef, changes, readBaseContent)
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
	reg.Register(checks.NewUntestedSymbolCheck(sqlite.NewCoverageRepo(pools.ReadDB),
		checks.WithUntestedRepoKindLookup(repoKind),
		checks.WithUntestedInterfaceMethods(sqlite.NewDeadCodeRepo(pools.ReadDB)),
	))
	return checks.NewRunner(reg, sqlite.NewFindingRepo(pools.Write), nil)
}

// fullCheckPass runs the structural checks over EVERY file in (repoID, branch)
// making absence-triggered rules like dead-code sound across files the change
// did not touch directly - then reads back the open structural finding ids.
func fullCheckPass(ctx context.Context, pools *sqlite.Pools, runner *checks.Runner, repoID, branch, gitSHA string) ([]string, error) {
	files, err := allFiles(ctx, pools.ReadDB, repoID, branch)
	if err != nil {
		return nil, err
	}
	runner.Run(ctx, checks.Input{RepoID: repoID, Branch: branch, GitSHA: gitSHA, FilePaths: files})
	return openStructuralFindingIDs(ctx, pools.ReadDB, repoID, branch)
}

// repromoteChanged stages the CHANGED files over the clone and promotes them as
// one batch through the real Promoter. The promoter rebinds a changed file's
// calls into unchanged same-package siblings against the already-promoted graph
// so a partial batch no longer drops cross-file edges. A file
// mapped to nil content stages no nodes (a deletion). Checks are NOT run during
// promotion - fullCheckPass runs them over the whole graph afterwards.
func repromoteChanged(ctx context.Context, pools *sqlite.Pools, repoID, branch, gitSHA string, changedFiles map[string][]byte) error {
	core := composition.NewColdScanCore(pools, nil, nil)
	for path, content := range changedFiles {
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
// only - a base-promotion finding from another rule (parse-failure, auto-link)
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
