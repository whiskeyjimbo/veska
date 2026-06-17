// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// checkVulnProvider gates daemon startup on the [vuln_source] provider.
// The vulnerability-scan feature ships off by default: an empty provider
// leaves the daemon on the NullVulnSource. The only enabled provider is "osv";
// any other value is a fatal startup error so an operator does not silently
// run with an unimplemented backend.
func checkVulnProvider(cfg config.Config) error {
	provider := cfg.VulnSource.Provider
	if provider == "" || provider == "osv" {
		return nil
	}
	return fmt.Errorf(
		"daemon: vuln_source.provider %q is not supported: only 'osv' is available",
		provider,
	)
}

// buildVulnSource constructs the ports.VulnSource for the resolved config and
// reports whether the vulnerability-scan feature is enabled.
// An empty [vuln_source] provider yields the NullVulnSource with enabled
// false - no refresher goroutine, no vulnscan check. provider = "osv" yields
// the OSV.dev-backed adapter with enabled true. The caller is expected to have
// run checkVulnProvider first, so an unrecognised provider also falls back to
// the NullVulnSource here rather than panicking.
func buildVulnSource(cfg config.Config) (ports.VulnSource, bool) {
	return composition.BuildVulnSource(cfg)
}

// vulnRefreshInterval parses the [vuln_source] refresh_interval. An empty or
// unparseable value yields a zero duration so the refresher keeps its default
// cadence.
func vulnRefreshInterval(cfg config.Config) time.Duration {
	if cfg.VulnSource.RefreshInterval == "" {
		return 0
	}
	d, err := time.ParseDuration(cfg.VulnSource.RefreshInterval)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// scanAllReposForVuln runs the vuln-scan check against every registered repo
// using the captured *VulnScanCheck and FindingStorage. Wired as the
// Refresher's first-refresh-ok callback so a freshly enabled [vuln_source]
// (or a freshly started daemon catching up on a stale cache) doesn't leave
// existing repos at "0 findings" until their next commit.
// Per-repo failures are logged and swallowed - the sweep is best-effort and
// must never crash the refresher goroutine. The active branch falls back to
// "main" so a repo that hasn't been promoted yet (active_branch="") still
// gets scanned.
func (d *Daemon) scanAllReposForVuln(ctx context.Context) {
	if d.vulnScanCheck == nil || d.findings == nil {
		return
	}
	records, err := repo.List(ctx, d.pools.ReadDB)
	if err != nil {
		slog.Warn("vuln-scan: scan-all-repos: list failed", "err", err)
		return
	}
	scanned := 0
	for _, rec := range records {
		branch := rec.ActiveBranch
		if branch == "" {
			branch = "main"
		}
		out, runErr := d.vulnScanCheck.Run(ctx, checks.Input{
			RepoID: rec.RepoID,
			Branch: branch,
		})
		if runErr != nil {
			slog.Warn("vuln-scan: scan-all-repos: check run failed",
				"repo_id", rec.RepoID, "err", runErr)
			continue
		}
		for _, f := range out {
			if f == nil {
				continue
			}
			if saveErr := d.findings.Save(ctx, f); saveErr != nil {
				slog.Warn("vuln-scan: scan-all-repos: persist finding failed",
					"repo_id", rec.RepoID, "err", saveErr)
			}
		}
		scanned++
	}
	slog.Info("vuln-scan: scan-all-repos complete",
		"repos_scanned", scanned, "repos_total", len(records))
}
