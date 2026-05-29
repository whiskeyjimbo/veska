package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// reindexDaemonProbe reports whether the daemon socket is reachable. It is
// a package-level seam so tests can simulate "daemon up" without standing
// up a real Unix socket. Production routes through daemonRunning().
var reindexDaemonProbe = daemonRunning

// dialReindex routes the reindex through the daemon's eng_reindex_repo MCP
// tool (solov2-4d7b) so the user does not have to stop the daemon before
// reindexing. Package-level seam so tests can swap a spy.
var dialReindex = defaultDialReindex

// resolveTargetForDial converts the user-supplied target into a (repoID,
// rootPath) pair suitable for eng_reindex_repo. When target is empty, the
// CWD is used as the rootPath (daemon canonicalises). A non-empty target
// is passed through as repoID first; the daemon falls back to NotFound
// rather than the CLI guessing, which keeps the resolution rules in one
// place. A target that exists as a directory is sent as rootPath instead
// so the daemon resolves by path.
func resolveTargetForDial(_ context.Context, target string) (repoID, rootPath string, err error) {
	if target == "" {
		cwd, werr := os.Getwd()
		if werr != nil {
			return "", "", fmt.Errorf("reindex: getwd: %w", werr)
		}
		return "", cwd, nil
	}
	if info, serr := os.Stat(target); serr == nil && info.IsDir() {
		abs, aerr := filepath.Abs(target)
		if aerr != nil {
			return "", "", fmt.Errorf("reindex: abs %q: %w", target, aerr)
		}
		return "", abs, nil
	}
	return target, "", nil
}

// defaultDialReindex sends the eng_reindex_repo RPC to the daemon. Either
// repoID or rootPath may be empty; the handler accepts either form.
func defaultDialReindex(ctx context.Context, repoID, rootPath string) (string, error) {
	type result struct {
		RepoID string `json:"repo_id"`
		Branch string `json:"branch"`
		Status string `json:"status"`
	}
	params := map[string]any{}
	if repoID != "" {
		params["repo_id"] = repoID
	}
	if rootPath != "" {
		params["root_path"] = rootPath
	}
	var r result
	if err := callMCP(ctx, "eng_reindex_repo", params, &r); err != nil {
		return "", err
	}
	return r.RepoID, nil
}

// reparserFactory builds a cold-scan reparser closure from an open SQLite
// pool set and an IgnoreLoader. It is a package-level seam so tests can
// substitute a spy that records invocations without spinning the real
// ingester/promoter/sinks pipeline up.
//
// Production path: defaultReparserFactory — wires Ingester + Promoter +
// PromotionStore + sinks (FTSSink, EmbedRefSink) exactly as
// cmd/veska-daemon/wire.go does. The duplication is intentional and mirrors
// the cmd/veska/wiki.go pattern; see follow-up bead noted via bd remember
// for the eventual extraction of a shared helper.
var reparserFactory = defaultReparserFactory

func defaultReparserFactory(pools *sqlite.Pools, loader application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	// Shared ingestion+promotion core (staging→ingester, promotionStore with
	// the FTS + embedding-ref sinks, promoter). Defined once in
	// internal/composition so it can no longer drift from the daemon's
	// cold-scan wiring (solov2-u4mv). reviewEnabled=false: the CLI path never
	// enqueues review work.
	core := composition.NewColdScanCore(pools, false)

	// solov2-izh6.16: install the post-promotion check pipeline (secret-scan
	// always, vuln-scan when [vuln_source] is enabled). Without this, a
	// `veska search --repo <url>` ephemeral clone — or any `veska reindex`
	// while the daemon is down — promotes without running vuln/secret checks,
	// so `veska findings list` is silently empty until the user reindexes
	// through the daemon. Findings flow through the same FindingStorage the
	// daemon uses, so subsequent `findings list` invocations see the rows.
	installColdScanCheckPipeline(core.Promoter, pools)

	reparser, err := application.NewColdScanReparser(
		core.Ingester, core.Promoter, gitwatch.Querier{},
		application.WithIgnoreLoader(loader),
	)
	if err != nil {
		return nil, fmt.Errorf("reindex: build cold-scan reparser: %w", err)
	}
	return reparser, nil
}

// installColdScanCheckPipeline wires Promoter.SetCheckRunner with the
// secret-leak + vuln-scan checks (per resolved config) and installs the
// AddedLinesFunc that drives the secret-scan rule. Mirrors the daemon's
// wire.go layout so the in-process cold-scan path emits findings on the
// FIRST promotion of an ephemeral or freshly-added repo (solov2-izh6.16).
//
// Errors during config load fall back to "secret-scan only" — vuln-scan is
// off by default anyway and a malformed config.toml should not silently
// disable secret detection on the cold-scan path.
var installColdScanCheckPipeline = func(promoter *application.Promoter, pools *sqlite.Pools) {
	findings := sqlite.NewFindingRepo(pools.Write)

	// AddedLines seam: the cold-scan promotion path runs against the repo
	// at its current HEAD, so resolve the diff for that SHA via the same
	// git.AddedLinesForCommit helper the daemon uses. Failure is non-fatal:
	// the runner just skips diff-driven checks for this promotion.
	root := repoRootByID(pools.ReadDB)
	promoter.SetAddedLinesFunc(composition.GitAddedLinesFunc(root))

	reg := checks.NewRegistry()

	// Secrets-scan ships on by default; only respect an explicit
	// disabled_checks entry. Config load failure falls through to "on".
	fileCfg, _ := config.Load()
	if !fileCfg.Promotion.CheckDisabled("secrets-scan") {
		reg.Register(checks.NewSecretsScanCheck(secretsscanner.New()))
	}

	// Vuln-scan only when [vuln_source] provider="osv" (matches daemon).
	if fileCfg.VulnSource.Provider == "osv" {
		src := buildCLIVulnSource(fileCfg)
		reg.Register(checks.NewVulnScanCheck(src, checks.RepoRootFunc(root)))
	}

	metrics := observability.NewMetrics(prometheus.NewRegistry())
	runner := checks.NewRunner(reg, findings, metrics)
	promoter.SetCheckRunner(composition.CheckRunnerAdapter{Inner: runner})
}

// buildCLIVulnSource mirrors daemon.buildVulnSource for the in-process
// cold-scan path. Returns NullVulnSource if config doesn't enable osv.
func buildCLIVulnSource(cfg config.Config) ports.VulnSource {
	if cfg.VulnSource.Provider != "osv" {
		return vulnsource.NewNullVulnSource()
	}
	return osv.New(osv.WithCacheDir(config.DefaultOSVCacheDir()))
}

// repoRootByID resolves a repoID to its registered working-tree path via
// the repos table — the cold-scan equivalent of daemon.repoRootFunc.
func repoRootByID(db *sql.DB) func(ctx context.Context, repoID string) (string, error) {
	return func(ctx context.Context, repoID string) (string, error) {
		records, err := repo.List(ctx, db)
		if err != nil {
			return "", fmt.Errorf("repo root lookup: %w", err)
		}
		for _, rec := range records {
			if rec.RepoID == repoID {
				return rec.RootPath, nil
			}
		}
		return "", fmt.Errorf("repo root lookup: repo %q is not registered", repoID)
	}
}

// reindexCmd returns the "reindex" Cobra command. It runs a full cold-scan
// reparse of the named (or cwd-resolved) repo unconditionally — bypassing
// the daemon's StartupResync gate that skips at-HEAD repos. This is the
// on-demand path that repopulates nodes.snippet and re-enqueues every
// promoted node for embedding against the live +snippet projection.
//
// The CLI opens the SQLite pools itself; if the daemon is running and
// holds a busy write transaction, this command will block on the write
// lock until the daemon releases it (sqlite WAL allows concurrent reads
// but one writer at a time).
func reindexCmd() *cobra.Command {
	var repoFlag string
	c := &cobra.Command{
		Use:          "reindex [<repo-id-or-path>]",
		Short:        "Force a full cold-scan reparse of a repository",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			ctx := cmd.Context()

			// resolveReindexFlagTarget: positional arg wins if both supplied
			// (with a stderr note), --repo is used when no positional given.
			target := resolveReindexFlagTarget(cmd.ErrOrStderr(), args, repoFlag)

			// solov2-4d7b: when the daemon is up, route the reindex
			// through its eng_reindex_repo MCP tool. The previous behaviour
			// (refuse with a stop-the-daemon hint, solov2-mdn3) disconnects
			// the editor's MCP session and was a junior-hostile regression
			// from add-time scans (which run inside the daemon already).
			// The direct-sqlite fallback below still handles the no-daemon
			// case.
			if reindexDaemonProbe() {
				repoID, rootPath, derr := resolveTargetForDial(ctx, target)
				if derr != nil {
					return derr
				}
				fmt.Fprintf(w, "reindexing via daemon...\n")
				gotID, err := dialReindex(ctx, repoID, rootPath)
				if err != nil {
					return fmt.Errorf("reindex: %w", err)
				}
				if gotID == "" {
					gotID = repoID
				}
				fmt.Fprintf(w, "reindex complete: repo %s\n", gotID)
				return nil
			}

			dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
			// Apply migrations so the schema is present even on a freshly
			// created database; mirror the wiki command's belt-and-braces
			// open-twice pattern.
			if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
				return fmt.Errorf("reindex: migrate sqlite: %w", err)
			}
			pools, err := sqlite.OpenPools(dbPath)
			if err != nil {
				return fmt.Errorf("reindex: open sqlite pools: %w", err)
			}
			defer func() { _ = pools.Close() }()

			rec, err := resolveReindexTarget(ctx, pools.ReadDB, target)
			if err != nil {
				return err
			}

			loader := func(repoRoot string) (application.IgnoreMatcher, error) {
				return fsignore.Load(repoRoot)
			}
			reparser, err := reparserFactory(pools, loader)
			if err != nil {
				return err
			}

			appRec := application.RepoRecord{
				RepoID:          rec.RepoID,
				RootPath:        rec.RootPath,
				ActiveBranch:    rec.ActiveBranch,
				LastPromotedSHA: rec.LastPromotedSHA,
			}
			if appRec.ActiveBranch == "" {
				appRec.ActiveBranch = "main"
			}

			fmt.Fprintf(w, "reindexing %s at %s...\n", appRec.RepoID, appRec.RootPath)
			if err := reparser(ctx, appRec); err != nil {
				return fmt.Errorf("reindex: %w", err)
			}

			// Best-effort HEAD lookup for the trailing message; an error
			// here does not invalidate the reindex itself.
			head, _ := gitwatch.Querier{}.HEAD(appRec.RootPath)
			fmt.Fprintf(w, "reindex complete: repo %s at SHA %s\n", appRec.RepoID, head)
			return nil
		},
	}
	c.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, alias or path (alias for the positional arg)")
	return c
}

// resolveReindexFlagTarget combines the positional arg and --repo flag into a
// single target string. The positional form wins when both are supplied; a
// one-line stderr note flags the override so a CI invocation that ends up
// passing both doesn't silently drop one.
func resolveReindexFlagTarget(stderr io.Writer, args []string, repoFlag string) string {
	var positional string
	if len(args) == 1 {
		positional = args[0]
	}
	if positional != "" && repoFlag != "" && positional != repoFlag {
		fmt.Fprintf(stderr, "reindex: positional arg %q overrides --repo %q\n", positional, repoFlag)
		return positional
	}
	if positional != "" {
		return positional
	}
	return repoFlag
}

// resolveReindexTarget picks the repo to reindex. With no arg, the cwd is
// canonicalised and matched against every registered repo's RootPath; with
// an arg, it is treated first as a repo id (repo.Get) and, on miss, as a
// path (canonicalised + matched against List).
func resolveReindexTarget(ctx context.Context, db *sql.DB, target string) (repo.Record, error) {
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return repo.Record{}, fmt.Errorf("reindex: getwd: %w", err)
		}
		return matchByPath(ctx, db, cwd)
	}

	// Try as a full id, short_id, or unambiguous prefix (solov2-c7lq).
	records, lerr := repo.List(ctx, db)
	if lerr != nil {
		return repo.Record{}, fmt.Errorf("reindex: list repos: %w", lerr)
	}
	if rec, rerr := resolveCLIRepoID(records, target); rerr == nil {
		return rec, nil
	}

	// Then as a filesystem path.
	if _, statErr := os.Stat(target); statErr == nil {
		return matchByPath(ctx, db, target)
	}
	return repo.Record{}, fmt.Errorf("reindex: repo %q is not registered (not a known id and not a registered path)", target)
}

// matchByPath canonicalises path with EvalSymlinks (matching the repo
// registry's stored form) and returns the registered repo whose RootPath
// equals it. An unregistered path is a typed error.
func matchByPath(ctx context.Context, db *sql.DB, path string) (repo.Record, error) {
	canonical, err := filepath.Abs(path)
	if err != nil {
		return repo.Record{}, fmt.Errorf("reindex: abs %q: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}

	records, err := repo.List(ctx, db)
	if err != nil {
		return repo.Record{}, fmt.Errorf("reindex: list repos: %w", err)
	}
	for _, r := range records {
		if r.RootPath == canonical {
			return r, nil
		}
	}
	return repo.Record{}, fmt.Errorf("reindex: path %q is not a registered repository", canonical)
}
