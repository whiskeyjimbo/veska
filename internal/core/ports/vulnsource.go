package ports

import "context"

// Advisory represents a single published security advisory that affects one or
// more packages. Fields mirror the minimal set needed by application-layer
// callers; implementations may populate additional data via extension types.
type Advisory struct {
	// ID is the stable identifier for this advisory (e.g. "CVE-2024-12345",
	// "GHSA-xxxx-yyyy-zzzz").
	ID string

	// Severity is a human-readable severity label (e.g. "CRITICAL", "HIGH",
	// "MEDIUM", "LOW"). The exact vocabulary is implementation-defined.
	Severity string

	// Summary is a short description of the vulnerability.
	Summary string

	// AffectedPackages lists the packages (module paths, npm names, etc.) that
	// are affected by this advisory.
	AffectedPackages []string
}

// VulnSource is the port for querying published security advisories.
// Implementations are provided by infrastructure adapters (e.g. OSV.dev,
// GitHub Advisory Database, NVD). The null implementation is used when
// vulnerability scanning is disabled.
type VulnSource interface {
	// Advisories returns all known advisories that affect pkg. pkg is an
	// ecosystem-specific package identifier (e.g. a Go module path or an
	// npm package name). An empty slice and a nil error means no advisories
	// were found. A nil implementation always returns nil, nil.
	Advisories(ctx context.Context, pkg string) ([]Advisory, error)
}
