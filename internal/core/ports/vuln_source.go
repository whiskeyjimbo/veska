// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// Dependency identifies a resolved dependency to be checked.
type Dependency struct {
	Ecosystem string
	Name      string
	Version   string
}

// VulnFinding represents an advisory that matched a scanned dependency.
type VulnFinding struct {
	AdvisoryID    string
	Package       string
	AffectedRange string
	Severity      string
	Summary       string

	// FixedVersion is the lowest version that resolves this advisory for the matching
	// affected range. It is empty when the advisory has no published fix yet.
	FixedVersion string

	// Aliases lists other advisory IDs that describe the same vulnerability. The OSV
	// adapter uses this to collapse duplicate advisories, which are kept on the
	// retained finding so triage can cross-check.
	Aliases []string
}

// VulnSource splits cache refresh from offline scanning so that network egress
// is confined to Refresh.
type VulnSource interface {
	// Refresh writes the advisory cache to disk. This is the only operation
	// that performs network egress. A nil implementation is a no-op.
	Refresh(ctx context.Context) error

	// Scan matches dependencies against the on-disk advisory cache and performs
	// no network I/O.
	Scan(ctx context.Context, deps []Dependency) ([]VulnFinding, error)
}
