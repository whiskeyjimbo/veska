package vulnsource_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource"
)

// Compile-time interface satisfaction check.
var _ ports.VulnSource = (*vulnsource.NullVulnSource)(nil)

func TestNullVulnSource_RefreshReturnsNil(t *testing.T) {
	t.Parallel()
	vs := vulnsource.NewNullVulnSource()

	if err := vs.Refresh(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNullVulnSource_ScanReturnsNil(t *testing.T) {
	t.Parallel()
	vs := vulnsource.NewNullVulnSource()

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "0.17.0"},
	}
	got, err := vs.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings, got %v", got)
	}
}

func TestNullVulnSource_ScanEmptyDeps(t *testing.T) {
	t.Parallel()
	vs := vulnsource.NewNullVulnSource()

	got, err := vs.Scan(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings, got %v", got)
	}
}
