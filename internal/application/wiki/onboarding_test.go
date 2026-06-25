// SPDX-License-Identifier: AGPL-3.0-only

package wiki

import (
	"strings"
	"testing"
	"time"
)

func onboardingFixture() (EntryPointsReport, Report, []DependencyRef) {
	ep := EntryPointsReport{
		GeneratedAt: time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		EntryPoints: []EntryPoint{
			{SymbolName: "Serve", FilePath: "internal/server/serve.go", LineStart: 42, InboundCount: 9, Summary: "starts the HTTP server"},
			{SymbolName: "Parse", FilePath: "internal/config/parse.go", LineStart: 10, InboundCount: 4, Summary: "parses the config file"},
		},
	}
	hot := Report{
		Zones: []HotZone{
			{FilePath: "internal/server/serve.go", RecentChangeFrequency: 7, BlastRadius: 12, Score: 84},
			{FilePath: "internal/config/parse.go", RecentChangeFrequency: 3, BlastRadius: 5, Score: 15},
		},
	}
	deps := []DependencyRef{
		{Module: "github.com/spf13/cobra", Version: "v1.8.0", Language: "go", UsageCount: 20, TopSymbol: "cobra.Command"},
	}
	return ep, hot, deps
}

// TestRenderOnboarding_OrderingInvariant asserts the reading path is ordered
// entry-points -> hot-zones -> dependencies (the AC1 invariant).
func TestRenderOnboarding_OrderingInvariant(t *testing.T) {
	out := RenderOnboarding(onboardingFixture())

	epIdx := strings.Index(out, "Entry points")
	hotIdx := strings.Index(out, "Hot zones")
	depIdx := strings.Index(out, "Dependencies")
	if epIdx < 0 || hotIdx < 0 || depIdx < 0 {
		t.Fatalf("missing a section:\n%s", out)
	}
	if epIdx >= hotIdx || hotIdx >= depIdx {
		t.Errorf("sections out of order: entry=%d hot=%d dep=%d", epIdx, hotIdx, depIdx)
	}

	// Each section's items carry the expected detail.
	if !strings.Contains(out, "`internal/server/serve.go:42`") {
		t.Error("entry point missing file:line ref")
	}
	if !strings.Contains(out, "starts the HTTP server") {
		t.Error("entry point missing summary")
	}
	if !strings.Contains(out, "github.com/spf13/cobra") {
		t.Error("dependency missing module")
	}
}

// TestRenderOnboarding_Deterministic is the AC2 byte-determinism gate: the
// render is a pure function of its inputs, so two renders of the same state
// are byte-identical (no map-order leakage).
func TestRenderOnboarding_Deterministic(t *testing.T) {
	ep, hot, deps := onboardingFixture()
	first := RenderOnboarding(ep, hot, deps)
	for range 4 {
		if got := RenderOnboarding(ep, hot, deps); got != first {
			t.Fatalf("render not deterministic:\n--- first ---\n%s\n--- got ---\n%s", first, got)
		}
	}
}

func TestRenderOnboarding_EmptyState(t *testing.T) {
	out := RenderOnboarding(EntryPointsReport{}, Report{}, nil)
	for _, want := range []string{"No entry points", "No hot zones", "No external dependencies"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty render missing %q:\n%s", want, out)
		}
	}
}

func TestOnboardingPagePath_IsUnderDocsVeska(t *testing.T) {
	if !strings.HasPrefix(OnboardingPagePath, "docs/veska/") {
		t.Errorf("OnboardingPagePath = %q, want under docs/veska/", OnboardingPagePath)
	}
}
