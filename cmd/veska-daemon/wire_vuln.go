package main

import (
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
)

// checkVulnProvider gates daemon startup on the [vuln_source] provider.
//
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
//
// An empty [vuln_source] provider yields the NullVulnSource with enabled
// false — no refresher goroutine, no vulnscan check. provider = "osv" yields
// the OSV.dev-backed adapter with enabled true. The caller is expected to have
// run checkVulnProvider first, so an unrecognised provider also falls back to
// the NullVulnSource here rather than panicking.
func buildVulnSource(cfg config.Config) (ports.VulnSource, bool) {
	if cfg.VulnSource.Provider != "osv" {
		return vulnsource.NewNullVulnSource(), false
	}
	return osv.New(osv.WithCacheDir(config.DefaultOSVCacheDir())), true
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
