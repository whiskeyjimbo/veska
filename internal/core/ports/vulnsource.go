package ports

import "context"

// Dependency identifies a single resolved dependency to be checked against the
// advisory cache. Fields mirror the minimal set needed to match a package
// against published advisories.
type Dependency struct {
	// Ecosystem is the package ecosystem (e.g. "Go", "npm", "PyPI"). The exact
	// vocabulary follows the advisory source's ecosystem labels.
	Ecosystem string

	// Name is the ecosystem-specific package identifier (e.g. a Go module path
	// or an npm package name).
	Name string

	// Version is the resolved version of the dependency.
	Version string
}

// VulnFinding represents a single advisory that matched a scanned dependency.
// Fields mirror the minimal set needed by application-layer callers.
type VulnFinding struct {
	// AdvisoryID is the stable identifier for the advisory (e.g.
	// "CVE-2024-12345", "GHSA-xxxx-yyyy-zzzz").
	AdvisoryID string

	// Package is the affected package identifier (module path, npm name, etc.).
	Package string

	// AffectedRange is the version range affected by the advisory, in the
	// advisory source's range syntax.
	AffectedRange string

	// Severity is a human-readable severity label (e.g. "CRITICAL", "HIGH",
	// "MEDIUM", "LOW"). The exact vocabulary is implementation-defined.
	Severity string

	// Summary is a short description of the vulnerability.
	Summary string

	// FixedVersion is the lowest version (semver, with leading "v" for Go
	// modules) that resolves this advisory for the affected range that
	// matched. Empty when the advisory has no published fix yet. Used by
	// application/checks to render a remediation hint (solov2-gpvy).
	FixedVersion string
}

// VulnSource is the port for vulnerability scanning. It splits cache refresh
// from offline scanning so that network egress is confined to Refresh.
// Implementations are provided by infrastructure adapters (e.g. OSV.dev). The
// null implementation is used when vulnerability scanning is disabled.
type VulnSource interface {
	// Refresh writes the advisory cache to disk. This is the only operation
	// that performs network egress. A nil implementation is a no-op.
	Refresh(ctx context.Context) error

	// Scan matches deps against the on-disk advisory cache and returns any
	// findings. It performs no network I/O. An empty slice and a nil error
	// means no matches were found. A nil implementation always returns nil,
	// nil.
	Scan(ctx context.Context, deps []Dependency) ([]VulnFinding, error)
}
