// Package vulnsource provides VulnSource implementations for the veska module.
package vulnsource

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// NullVulnSource is a VulnSource that always returns no advisories. It is the
// default implementation and is used when vulnerability scanning is disabled.
//
// NullVulnSource is safe for concurrent use.
type NullVulnSource struct{}

// Compile-time interface satisfaction check.
var _ ports.VulnSource = (*NullVulnSource)(nil)

// NewNullVulnSource constructs a NullVulnSource.
func NewNullVulnSource() *NullVulnSource {
	return &NullVulnSource{}
}

// Advisories always returns nil, nil — no advisories found, no error.
func (n *NullVulnSource) Advisories(_ context.Context, _ string) ([]ports.Advisory, error) {
	return nil, nil
}
