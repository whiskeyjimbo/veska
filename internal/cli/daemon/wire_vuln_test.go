package daemon

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
)

// An absent [vuln_source] section (empty provider) is accepted and leaves the
// feature off.
func TestCheckVulnProvider_EmptyIsOff(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	if cfg.VulnSource.Provider != "" {
		t.Fatalf("precondition: default provider = %q, want empty", cfg.VulnSource.Provider)
	}
	if err := checkVulnProvider(cfg); err != nil {
		t.Fatalf("empty provider rejected: %v", err)
	}
}

// provider = "osv" is accepted.
func TestCheckVulnProvider_AcceptsOSV(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.VulnSource.Provider = "osv"
	if err := checkVulnProvider(cfg); err != nil {
		t.Fatalf("osv provider rejected: %v", err)
	}
}

// An unknown provider is a fatal startup error naming the supported value.
func TestCheckVulnProvider_RejectsUnknown(t *testing.T) {
	t.Parallel()

	for _, prov := range []string{"snyk", "github", "ghsa"} {
		cfg := config.DefaultConfig()
		cfg.VulnSource.Provider = prov
		err := checkVulnProvider(cfg)
		if err == nil {
			t.Fatalf("provider %q: expected error, got nil", prov)
		}
		if !strings.Contains(err.Error(), "osv") {
			t.Errorf("provider %q: error %q does not name the supported 'osv' provider", prov, err)
		}
	}
}

// With no provider configured, buildVulnSource yields the NullVulnSource and
// reports the feature off (no refresher, no check).
func TestBuildVulnSource_OffByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	src, enabled := buildVulnSource(cfg)
	if enabled {
		t.Error("buildVulnSource: feature should be off when provider is empty")
	}
	if _, ok := src.(*vulnsource.NullVulnSource); !ok {
		t.Errorf("buildVulnSource: want *NullVulnSource when off, got %T", src)
	}
}

// With provider = "osv", buildVulnSource yields a non-null source and reports
// the feature on.
func TestBuildVulnSource_OSVEnabled(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.VulnSource.Provider = "osv"
	src, enabled := buildVulnSource(cfg)
	if !enabled {
		t.Error("buildVulnSource: feature should be on when provider is osv")
	}
	if _, ok := src.(*vulnsource.NullVulnSource); ok {
		t.Error("buildVulnSource: want OSV adapter when on, got *NullVulnSource")
	}
}
