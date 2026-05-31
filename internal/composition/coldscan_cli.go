package composition

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
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

// NewCLIColdScanReparser builds the in-process cold-scan reparser used by
// `veska reindex` and the ephemeral-clone path of `veska search` when no
// daemon is running. It wires the shared ingestion/promotion core
// (NewColdScanCore) together with the CLI post-promotion check pipeline
// (secret-scan always, vuln-scan when [vuln_source] is enabled) so a cold
// scan emits findings on the FIRST promotion of an ephemeral or freshly-added
// repo, exactly as the daemon does.
//
// This was previously assembled inside cmd/veska/reindex.go; it lives here so
// the delivery layer carries no composition wiring and the check pipeline no
// longer hand-mirrors internal/cli/daemon/wire.go from a separate copy
// (solov2-0omh, follow-up to the SOLID/CLEAN review of cmd/veska).
func NewCLIColdScanReparser(pools *sqlite.Pools, loader application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	// reviewEnabled=false: the CLI path never enqueues review work. The CLI
	// ingester emits no parse findings (no ingesterOpts); only the promoter
	// carries the post-promotion check pipeline.
	core := NewColdScanCore(pools, false, nil, cliColdScanPromoterOpts(pools))

	reparser, err := application.NewColdScanReparser(
		core.Ingester, core.Promoter, gitwatch.Querier{},
		application.WithIgnoreLoader(loader),
	)
	if err != nil {
		return nil, fmt.Errorf("cold-scan reparser: %w", err)
	}
	return reparser, nil
}

// cliColdScanPromoterOpts builds the Promoter options for the CLI cold-scan
// path: the secret-leak + vuln-scan check runner (per resolved config) and the
// AddedLinesFunc that drives the secret-scan rule.
//
// Errors during config load fall back to "secret-scan only" — vuln-scan is off
// by default anyway and a malformed config.toml should not silently disable
// secret detection on the cold-scan path.
func cliColdScanPromoterOpts(pools *sqlite.Pools) []application.PromoterOption {
	findings := sqlite.NewFindingRepo(pools.Write)

	// AddedLines seam: the cold-scan promotion path runs against the repo at
	// its current HEAD, so resolve the diff for that SHA via the same
	// git.AddedLinesForCommit helper the daemon uses. Failure is non-fatal:
	// the runner just skips diff-driven checks for this promotion.
	root := RepoRootByID(pools.ReadDB)

	reg := checks.NewRegistry()

	// Secrets-scan ships on by default; only respect an explicit
	// disabled_checks entry. Config load failure falls through to "on".
	fileCfg, _ := config.Load()
	if !fileCfg.Promotion.CheckDisabled("secrets-scan") {
		reg.Register(checks.NewSecretsScanCheck(secretsscanner.New()))
	}

	// Vuln-scan only when [vuln_source] is enabled (provider="osv"). The CLI
	// path discards the enabled bool — it only registers the check when a
	// non-null source is returned.
	if src, enabled := BuildVulnSource(fileCfg); enabled {
		reg.Register(checks.NewVulnScanCheck(src, checks.RepoRootFunc(root)))
	}

	metrics := observability.NewMetrics(prometheus.NewRegistry())
	runner := checks.NewRunner(reg, findings, metrics)
	return []application.PromoterOption{
		application.WithAddedLinesFunc(GitAddedLinesFunc(root)),
		application.WithCheckRunner(CheckRunnerAdapter{Inner: runner}),
	}
}

// BuildVulnSource constructs the ports.VulnSource for the resolved config and
// reports whether the vulnerability-scan feature is enabled. It is the single
// source of the provider-switch rule shared by the in-process cold-scan path
// (cliColdScanPromoterOpts) and the daemon (daemon.buildVulnSource).
//
// An empty or unrecognised [vuln_source] provider yields the NullVulnSource
// with enabled false — no refresher goroutine, no vuln-scan check. provider =
// "osv" yields the OSV.dev-backed adapter with enabled true. Daemon callers are
// expected to have run checkVulnProvider first, so an unrecognised provider
// falls back to the NullVulnSource here rather than panicking.
func BuildVulnSource(cfg config.Config) (ports.VulnSource, bool) {
	if cfg.VulnSource.Provider != "osv" {
		return vulnsource.NewNullVulnSource(), false
	}
	return osv.New(osv.WithCacheDir(config.DefaultOSVCacheDir())), true
}

// RepoRootByID resolves a repoID to its registered working-tree path via the
// repos table. It is the single canonical resolver shared by the cold-scan CLI
// and the daemon (whose repoRootFunc converts it to mcp.RepoRootFunc). An
// unknown repoID yields an error so callers surface a clear "repo not
// registered" message rather than running against an empty path.
func RepoRootByID(db *sql.DB) func(ctx context.Context, repoID string) (string, error) {
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
