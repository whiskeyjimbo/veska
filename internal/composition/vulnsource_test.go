// SPDX-License-Identifier: AGPL-3.0-only

package composition

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// With no provider configured, BuildVulnSource yields the NullVulnSource and
// reports the feature off.
func TestBuildVulnSource_OffByDefault(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	src, enabled := BuildVulnSource(cfg)
	if enabled {
		t.Error("BuildVulnSource: feature should be off when provider is empty")
	}
	if _, ok := src.(*vulnsource.NullVulnSource); !ok {
		t.Errorf("BuildVulnSource: want *NullVulnSource when off, got %T", src)
	}
}

// An unrecognized provider falls back to the NullVulnSource.
func TestBuildVulnSource_UnknownIsOff(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.VulnSource.Provider = "snyk"
	src, enabled := BuildVulnSource(cfg)
	if enabled {
		t.Error("BuildVulnSource: feature should be off for unknown provider")
	}
	if _, ok := src.(*vulnsource.NullVulnSource); !ok {
		t.Errorf("BuildVulnSource: want *NullVulnSource when off, got %T", src)
	}
}

// With provider set to "osv", BuildVulnSource yields a non-null source and reports
// the feature on.
func TestBuildVulnSource_OSVEnabled(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.VulnSource.Provider = "osv"
	src, enabled := BuildVulnSource(cfg)
	if !enabled {
		t.Error("BuildVulnSource: feature should be on when provider is osv")
	}
	if _, ok := src.(*vulnsource.NullVulnSource); ok {
		t.Error("BuildVulnSource: want OSV adapter when on, got *NullVulnSource")
	}
}
