package composition

import (
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// RegisterCommonChecks installs the post-promotion checks shared by both the
// in-process cold-scan CLI path and the daemon: secrets-scan (on unless
// [promotion] disabled_checks lists "secrets-scan") and vuln-scan (only when
// the vulnerability source is enabled, i.e. provider="osv").
// It is the single copy of those two enablement rules; callers that need extra
// checks (the daemon registers dead-code and contract-drift) layer them on top
// of the same registry. vulnSource/vulnEnabled are passed in rather than
// recomputed so the daemon can feed the SAME source instance to both the check
// and its advisory-cache refresher.
// When vuln-scan is enabled the constructed *checks.VulnScanCheck is returned
// (else nil) so callers that retain a reference to it (the daemon stores
// b.vulnScanCheck for its targeted re-run path) keep that field populated
// without re-deriving the policy.
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
