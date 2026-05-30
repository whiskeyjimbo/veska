package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/cli/reindexcmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// The reindex dispatch + resolution logic lives in internal/cli/reindexcmd;
// this RunE merges the positional/--repo target and delegates, injecting the
// cmd-owned cold-scan seams (reparserFactory, matchByPath) it shares with
// `veska search` (solov2-0omh).

// reparserFactory builds a cold-scan reparser closure from an open SQLite pool
// set and an IgnoreLoader. It is a cmd-owned seam shared with `veska search`
// (searchcmd.RunOpts.ReparserFactory) and injected into reindexcmd.Params so
// tests can substitute a spy that records invocations without spinning the real
// ingester/promoter/sinks pipeline up.
//
// Production path: defaultReparserFactory — wires Ingester + Promoter +
// PromotionStore + sinks (FTSSink, EmbedRefSink) exactly as
// cmd/veska-daemon/wire.go does.
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
func installColdScanCheckPipeline(promoter *application.Promoter, pools *sqlite.Pools) {
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

// matchByPath canonicalises path with EvalSymlinks (matching the repo
// registry's stored form) and returns the registered repo whose RootPath
// equals it. An unregistered path is a typed error. It is a cmd-owned seam
// shared with `veska search` (searchcmd.RunOpts.MatchByPath) and reindexcmd.
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

// reindexCmd returns the "reindex" Cobra command. It forces a full cold-scan
// reparse of the named (or cwd-resolved) repo; reindexcmd.Run owns the
// daemon-dispatch fork and the direct-SQLite fallback.
func reindexCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:          "reindex [<repo-id-or-path>]",
		Short:        "Force a full cold-scan reparse of a repository",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// MergeTarget: positional arg wins if both supplied (with a
			// stderr note), --repo is used when no positional given.
			target := reindexcmd.MergeTarget(cmd.ErrOrStderr(), args, repoFlag)
			return reindexcmd.Run(cmd.Context(), reindexcmd.Params{
				Target:          target,
				Out:             cmd.OutOrStdout(),
				ErrOut:          cmd.ErrOrStderr(),
				DaemonRunning:   daemonRunning,
				DialReindex:     reindexcmd.DefaultDial,
				ReparserFactory: reparserFactory,
				MatchByPath:     matchByPath,
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, alias or path (alias for the positional arg)")
	return cmd
}
