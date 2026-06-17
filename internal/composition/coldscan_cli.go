package composition

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// NewCLIColdScanReparser builds the in-process cold-scan reparser used when no
// daemon is running. It wires the shared ingestion and promotion core with the
// CLI post-promotion check pipeline so a cold scan emits findings on the first
// promotion of a repository.
func NewCLIColdScanReparser(pools *sqlite.Pools, loader application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	// reviewEnabled=false: the CLI path never enqueues review work. The CLI
	// ingester emits no parse findings (no ingesterOpts); only the promoter
	// carries the post-promotion check pipeline.
	core := NewColdScanCore(pools, nil, cliColdScanPromoterOpts(pools))

	return NewColdScanReparser(core.Ingester, core.Promoter, loader)
}

// cliColdScanPromoterOpts builds the Promoter options for the CLI cold-scan
// path. If config loading fails, it falls back to running only the secrets-scan
// check to ensure secret detection is not silently disabled.
func cliColdScanPromoterOpts(pools *sqlite.Pools) []application.PromoterOption {
	findings := sqlite.NewFindingRepo(pools.Write)

	// AddedLines seam resolves the diff for the current HEAD commit using
	// git.AddedLinesForCommit. Failure is non-fatal: the runner skips
	// diff-driven checks on failure.
	root := RepoRootByID(pools.ReadDB)

	reg := checks.NewRegistry()

	fileCfg, _ := config.Load()
	vulnSource, vulnEnabled := BuildVulnSource(fileCfg)
	RegisterCommonChecks(reg, fileCfg, vulnSource, vulnEnabled, checks.RepoRootFunc(root))

	metrics := observability.NewMetrics(prometheus.NewRegistry())
	runner := checks.NewRunner(reg, findings, metrics, checks.WithLogger(slog.Default()))
	return []application.PromoterOption{
		application.WithAddedLinesFunc(GitAddedLinesFunc(root)),
		application.WithCheckRunner(CheckRunnerAdapter{Inner: runner}),
	}
}

// BuildVulnSource constructs the ports.VulnSource for the resolved configuration
// and reports whether the vulnerability-scan feature is enabled. An unrecognized
// provider defaults to a NullVulnSource to prevent panics.
func BuildVulnSource(cfg config.Config) (ports.VulnSource, bool) {
	if cfg.VulnSource.Provider != "osv" {
		return vulnsource.NewNullVulnSource(), false
	}
	return osv.New(osv.WithCacheDir(config.DefaultOSVCacheDir())), true
}

// RepoRootByID resolves a repoID to its registered working-tree path via the
// repos table. An unknown repository ID yields an error to prevent running
// against an empty path.
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
