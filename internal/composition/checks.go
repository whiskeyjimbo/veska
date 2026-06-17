package composition

import (
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// RegisterCommonChecks installs the post-promotion checks shared by the
// cold-scan path and the daemon. vulnSource and vulnEnabled are passed in so
// the daemon can share the same source instance between the check and its cache
// refresher. It returns the constructed VulnScanCheck so callers can retain a
// reference to it for targeted re-runs.
func RegisterCommonChecks(reg *checks.Registry, fileCfg config.Config, vulnSource ports.VulnSource, vulnEnabled bool, repoRoot checks.RepoRootFunc) *checks.VulnScanCheck {
	if !fileCfg.Promotion.CheckDisabled("secrets-scan") {
		reg.Register(checks.NewSecretsScanCheck(secretsscanner.New()))
	}
	if !vulnEnabled {
		return nil
	}
	vulnCheck := checks.NewVulnScanCheck(vulnSource, repoRoot)
	reg.Register(vulnCheck)
	return vulnCheck
}
