// SPDX-License-Identifier: AGPL-3.0-only

// Package vulnsource provides VulnSource implementations for the veska module.
package vulnsource

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// NullVulnSource is a no-op implementation of VulnSource. It is used when
// vulnerability scanning is disabled, and is safe for concurrent use.
type NullVulnSource struct{}

var _ ports.VulnSource = (*NullVulnSource)(nil)

func NewNullVulnSource() *NullVulnSource {
	return &NullVulnSource{}
}

func (n *NullVulnSource) Refresh(_ context.Context) error {
	return nil
}

func (n *NullVulnSource) Scan(_ context.Context, _ []ports.Dependency) ([]ports.VulnFinding, error) {
	return nil, nil
}
