// Package searchcmd holds the business logic behind the `veska search`
// command: the daemon-first / ephemeral-clone / in-process search paths, the
// {results, degraded_reasons} envelope build + render, the 'searching:'
// stderr header, ephemeral cache-tier path prettification, and the small
// classifiers/hints those paths rely on.
//
// cmd/veska/search.go is reduced to Cobra command construction whose RunE
// is a thin call into Run here (solov2-0omh.5, following the cmd = glue /
// logic-in-packages pattern from solov2-u4mv). Cross-package seams the cmd
// package owns — the cold-scan reparser factory and the cwd→repo matcher,
// both shared with `veska reindex` — are injected through RunOpts rather
// than re-extracted here.
package searchcmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// ReparserFactory builds a cold-scan reparser closure from an open SQLite
// pool set and an IgnoreLoader. The cmd package owns the production wiring
// (shared with `veska reindex`) and injects it through RunOpts so the search
// orchestration stays free of the ingester/promoter/sinks construction.
type ReparserFactory func(*sqlite.Pools, application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error)

// MatchByPath resolves a filesystem path to its registered repo Record. The
// cmd package owns this (shared with `veska reindex`) and injects it through
// RunOpts.
type MatchByPath func(ctx context.Context, db *sql.DB, path string) (repo.Record, error)

// RunOpts carries the parsed CLI inputs plus the cmd-owned seams the search
// orchestration delegates back into.
type RunOpts struct {
	Query   string
	Target  string
	K       int
	JSONOut bool

	// ReparserFactory + MatchByPath are the cmd-package seams shared with
	// `veska reindex`; injected so this package never re-wires them.
	ReparserFactory ReparserFactory
	MatchByPath     MatchByPath
}

// Run is the search command entry point. See cmd/veska/search.go for the
// flag/positional contract; this preserves the original runSearch behaviour
// byte-for-byte.
func Run(ctx context.Context, w, stderr io.Writer, opts RunOpts) error {
	// Daemon-first: when a daemon is up and already tracks the target repo,
	// run the query through its eng_search_semantic so the CLI shares the
	// daemon's hybrid (vector + lexical) retrieval pipeline and never opens
	// a second writer on veska.db (solov2-b1q, solov2-xkm). The in-process
	// path below is the fallback for when the daemon is down or the repo is
	// not yet registered (it clones/indexes synchronously).
	if env, handled, err := daemonSearch(ctx, stderr, w, opts); handled {
		if err != nil {
			return err
		}
		return RenderSearchEnvelope(w, env, opts.JSONOut)
	}

	pools, err := openSearchPools()
	if err != nil {
		return err
	}
	defer func() { _ = pools.Close() }()

	rec, err := resolveSearchTarget(ctx, pools, opts, w)
	if err != nil {
		return err
	}

	if err := ensureIndexed(ctx, pools, rec, opts, w); err != nil {
		return err
	}

	prettifier := ephemeralPrettifier(ctx, pools, rec, opts.JSONOut)

	// solov2-izh6.15: announce the resolved repo before running the
	// search. The in-process / URL-target flow always knows the exact
	// repo (rec) — show its short_id since aliases aren't loaded on
	// the Record here. JSON mode suppresses inside EmitSearchHeader.
	EmitSearchHeader(stderr, w, opts.JSONOut, SearchHeaderInfo{
		Mode:    SearchHeaderModeExplicit,
		RepoID:  rec.RepoID,
		ShortID: shortIDOf(rec.RepoID),
	})

	if err := runResolvedSearch(ctx, resolvedSearch{
		pools:      pools,
		rec:        rec,
		opts:       opts,
		prettifier: prettifier,
		w:          w,
	}); err != nil {
		return err
	}

	offerAcceptancePrompt(ctx, pools, rec, opts, w)
	return nil
}

// openSearchPools migrates and opens the local veska.db pool set used by the
// in-process search path.
func openSearchPools() (*sqlite.Pools, error) {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
		return nil, fmt.Errorf("search: migrate sqlite: %w", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return nil, fmt.Errorf("search: open sqlite pools: %w", err)
	}
	return pools, nil
}

// ephemeralPrettifier builds the file-path shortener for ephemeral cache-tier
// repos so the user sees `pflag/flag.go` instead of the 64-char-sha cache
// path. Returns nil for JSON output or tracked repos. solov2-l04m.
func ephemeralPrettifier(ctx context.Context, pools *sqlite.Pools, rec repo.Record, jsonOut bool) func(*SearchEnvelope) {
	if jsonOut || rec.Kind != "ephemeral" {
		return nil
	}
	canonical, _ := lookupCanonicalURL(ctx, pools.ReadDB, rec.RepoID)
	display := ephemeralDisplayName(canonical, rec.RepoID)
	// The on-disk clone dir is named by the URL-derived id (see
	// ephemeralEnsureFromURL), not the path-sha rec.RepoID stored in the
	// registry — rec.RootPath is the authoritative prefix.
	cacheRepoDir := rec.RootPath
	return func(env *SearchEnvelope) {
		prettifyEphemeralPaths(env, cacheRepoDir, display)
	}
}

// resolvedSearch bundles the inputs to the post-resolution search pass so the
// function stays under the argument-count gate.
type resolvedSearch struct {
	pools      *sqlite.Pools
	rec        repo.Record
	opts       RunOpts
	prettifier func(*SearchEnvelope)
	w          io.Writer
}

// runResolvedSearch runs the query against an already-resolved repo. It
// prefers the daemon's hybrid eng_search_semantic (solov2-2etd) and falls
// back to the in-process vector-only service when the daemon is unreachable.
func runResolvedSearch(ctx context.Context, rs resolvedSearch) error {
	if env, ok, derr := daemonSearchByRepoID(ctx, rs.rec.RepoID, rs.rec.ActiveBranch, rs.opts); ok {
		if derr != nil {
			return derr
		}
		if rs.prettifier != nil {
			rs.prettifier(&env)
		}
		return RenderSearchEnvelope(rs.w, env, rs.opts.JSONOut)
	}

	svc, err := composition.NewCLISearchService(rs.pools)
	if err != nil {
		return err
	}
	resp, err := svc.Semantic(ctx, rs.rec.RepoID, rs.rec.ActiveBranch, rs.opts.Query, rs.opts.K, domain.Filter{})
	if err != nil {
		return fmt.Errorf("search: semantic: %w", err)
	}
	env := buildSearchEnvelope(resp)
	if rs.prettifier != nil {
		rs.prettifier(&env)
	}
	return RenderSearchEnvelope(rs.w, env, rs.opts.JSONOut)
}

// offerAcceptancePrompt runs the post-search "keep this indexed?" prompt for
// ephemeral repos. No-op for tracked rows, JSON output, or already-prompted
// repos. Failure to record the outcome is non-fatal — the search succeeded;
// we log to stderr so the user knows the row stays ephemeral. solov2-kxo5.7.
func offerAcceptancePrompt(ctx context.Context, pools *sqlite.Pools, rec repo.Record, opts RunOpts, w io.Writer) {
	if opts.JSONOut || rec.Kind != "ephemeral" {
		return
	}
	canonical, _ := lookupCanonicalURL(ctx, pools.ReadDB, rec.RepoID)
	if canonical == "" {
		canonical = rec.RootPath
	}
	if err := RunAcceptancePrompt(ctx, pools.Write, rec, canonical, repocmd.DefaultPromptDeps(w)); err != nil {
		fmt.Fprintf(os.Stderr, "search: acceptance prompt: %v\n", err)
	}
}

// lookupCanonicalURL fetches canonical_url for repoID; empty string on any
// read error or NULL. Used by the acceptance prompt to address the user with
// the URL they actually typed instead of the cache-tier path.
func lookupCanonicalURL(ctx context.Context, db *sql.DB, repoID string) (string, error) {
	var s sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT canonical_url FROM repos WHERE repo_id = ?`, repoID,
	).Scan(&s); err != nil {
		return "", err
	}
	if !s.Valid {
		return "", nil
	}
	return s.String, nil
}

// resolveSearchTarget picks the repo the search will run against. The three
// input modes mirror the bead AC:
//
//   - empty arg: use the repo whose RootPath matches cwd (or the only
//     registered repo, if any). Doesn't clone or scan.
//   - a filesystem path: register if needed; subsequent runs reuse the
//     registration so the index survives.
//   - a git URL: clone to the cache tier on first use, reuse the same dir on
//     re-runs (AC2 — index reuse).
func resolveSearchTarget(ctx context.Context, pools *sqlite.Pools, opts RunOpts, w io.Writer) (repo.Record, error) {
	target := opts.Target
	if target == "" {
		return resolveCwdTarget(ctx, pools, opts)
	}
	if isGitURL(target) {
		// solov2-kxo5.6: URL targets route through the cache tier and land
		// as kind='ephemeral' rows so the eviction story works. A
		// canonical_url match (tracked or ephemeral) short-circuits the
		// clone — same code re-used between sessions hits the warm graph
		// instead of re-parsing.
		return ephemeralEnsureFromURL(ctx, pools, target, w)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		return findOrRegisterRepo(ctx, pools, opts, target)
	}
	return repo.Record{}, fmt.Errorf("search: target %q is neither an existing path nor a git URL", target)
}

// resolveCwdTarget handles the empty-target case: only run against an already-
// registered repo matching cwd. Auto-registering cwd here is a footgun —
// running `veska search` from /tmp or any non-git directory would otherwise
// cold-scan a random path (solov2-bbgj). The user must explicitly pass <path>
// or run `veska repo add` first.
func resolveCwdTarget(ctx context.Context, pools *sqlite.Pools, opts RunOpts) (repo.Record, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: getwd: %w", err)
	}
	rec, err := opts.MatchByPath(ctx, pools.ReadDB, cwd)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(cwd, ".git")); statErr != nil {
			return repo.Record{}, fmt.Errorf("search: cwd %q is not a git repository; pass <path> or cd to a registered repo", cwd)
		}
		return repo.Record{}, fmt.Errorf("search: %q is not registered; run `veska repo add %s` or pass it as <path>", cwd, cwd)
	}
	return rec, nil
}

func findOrRegisterRepo(ctx context.Context, pools *sqlite.Pools, opts RunOpts, path string) (repo.Record, error) {
	rec, err := opts.MatchByPath(ctx, pools.ReadDB, path)
	if err == nil {
		return rec, nil
	}
	// Not registered yet — add. Subsequent runs find the existing
	// registration (AC2: reuse the index).
	id, _, addErr := repo.Add(ctx, pools.Write, path)
	if addErr != nil {
		return repo.Record{}, fmt.Errorf("search: register repo %q: %w", path, addErr)
	}
	rec, err = repo.Get(ctx, pools.ReadDB, id)
	if err != nil {
		return repo.Record{}, fmt.Errorf("search: get newly-registered repo: %w", err)
	}
	if rec.ActiveBranch == "" {
		rec.ActiveBranch = "main"
	}
	return rec, nil
}

// branchOrMain defaults an empty branch to "main".
func branchOrMain(b string) string {
	if b == "" {
		return "main"
	}
	return b
}

// shortIDOf returns the 12-char prefix of a repo_id used in the registry's
// short_id column, or the input unchanged if it's already shorter.
func shortIDOf(repoID string) string {
	if len(repoID) > 12 {
		return repoID[:12]
	}
	return repoID
}
