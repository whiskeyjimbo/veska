// SPDX-License-Identifier: AGPL-3.0-only

package composition

import (
	"context"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

func stubRepoRoot(context.Context, string) (string, error) { return "", nil }

func TestRegisterCommonChecks_SecretsAndVuln(t *testing.T) {
	reg := checks.NewRegistry()
	cfg := config.Config{}
	src := vulnsource.NewNullVulnSource()

	RegisterCommonChecks(reg, cfg, src, true, stubRepoRoot)

	names := reg.Names()
	if !slices.Contains(names, "secrets-scan") {
		t.Errorf("expected secrets-scan registered, got %v", names)
	}
	if !slices.Contains(names, "vuln-scan") {
		t.Errorf("expected vuln-scan registered, got %v", names)
	}
}

func TestRegisterCommonChecks_SecretsDisabled(t *testing.T) {
	reg := checks.NewRegistry()
	cfg := config.Config{
		Promotion: config.PromotionConfig{DisabledChecks: []string{"secrets-scan"}},
	}
	src := vulnsource.NewNullVulnSource()

	RegisterCommonChecks(reg, cfg, src, true, stubRepoRoot)

	names := reg.Names()
	if slices.Contains(names, "secrets-scan") {
		t.Errorf("secrets-scan should NOT be registered when disabled, got %v", names)
	}
	if !slices.Contains(names, "vuln-scan") {
		t.Errorf("expected vuln-scan registered, got %v", names)
	}
}

func TestRegisterCommonChecks_VulnDisabled(t *testing.T) {
	reg := checks.NewRegistry()
	cfg := config.Config{}
	src := vulnsource.NewNullVulnSource()

	RegisterCommonChecks(reg, cfg, src, false, stubRepoRoot)

	names := reg.Names()
	if !slices.Contains(names, "secrets-scan") {
		t.Errorf("expected secrets-scan registered, got %v", names)
	}
	if slices.Contains(names, "vuln-scan") {
		t.Errorf("vuln-scan should NOT be registered when disabled, got %v", names)
	}
}

func TestRegisterCommonChecks_ReturnsVulnCheckWhenEnabled(t *testing.T) {
	reg := checks.NewRegistry()
	cfg := config.Config{}
	src := vulnsource.NewNullVulnSource()

	got := RegisterCommonChecks(reg, cfg, src, true, stubRepoRoot)
	if got == nil {
		t.Fatal("expected non-nil VulnScanCheck when vuln enabled")
	}

	if got := RegisterCommonChecks(checks.NewRegistry(), cfg, src, false, stubRepoRoot); got != nil {
		t.Fatalf("expected nil VulnScanCheck when vuln disabled, got %v", got)
	}
}
