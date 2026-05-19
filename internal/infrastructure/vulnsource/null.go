// Package vulnsource provides VulnSource implementations for the veska module.
package vulnsource

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// NullVulnSource is a VulnSource that performs no work: Refresh is a no-op and
// Scan reports no findings. It is the default implementation and is used when
// vulnerability scanning is disabled.
//
// NullVulnSource is safe for concurrent use.
type NullVulnSource struct{}

// Compile-time interface satisfaction check.
var _ ports.VulnSource = (*NullVulnSource)(nil)

// NewNullVulnSource constructs a NullVulnSource.
func NewNullVulnSource() *NullVulnSource {
	return &NullVulnSource{}
}

// Refresh is a no-op and always returns nil — no cache is written.
func (n *NullVulnSource) Refresh(_ context.Context) error {
	return nil
}

// Scan always returns nil, nil — no findings, no error.
func (n *NullVulnSource) Scan(_ context.Context, _ []ports.Dependency) ([]ports.VulnFinding, error) {
	return nil, nil
}
