package vulnsource_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
)

// Compile-time interface satisfaction check.
var _ ports.VulnSource = (*vulnsource.NullVulnSource)(nil)

func TestNullVulnSource_AdvisoriesReturnsNil(t *testing.T) {
	t.Parallel()
	vs := vulnsource.NewNullVulnSource()

	got, err := vs.Advisories(context.Background(), "golang.org/x/net")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil advisories, got %v", got)
	}
}

func TestNullVulnSource_AdvisoriesEmptyPkg(t *testing.T) {
	t.Parallel()
	vs := vulnsource.NewNullVulnSource()

	got, err := vs.Advisories(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil advisories, got %v", got)
	}
}
