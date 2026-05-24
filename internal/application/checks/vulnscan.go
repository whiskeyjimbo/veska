package checks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/manifest"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RepoRootFunc resolves a repoID to its registered working-tree path. It
// mirrors the review/wiki handler resolvers so the vulnscan check can turn a
// RepoID into a filesystem root and read its go.mod from disk.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// VulnScanCheck is a structural check that turns cached advisories into
// findings on promotion. When go.mod is among the promotion's changed files
// it reads the module's dependencies and scans them against the offline
// advisory cache, emitting one finding per matched advisory.
//
// The check is offline: dependency resolution is textual (manifest.ReadGoMod)
// and VulnSource.Scan performs no network I/O. Findings anchor on the go.mod
// path with a discriminator key of advisoryID+package, which makes the
// resulting finding_ids branch-stable and idempotent — re-running on
// unchanged state yields byte-identical finding_ids.
type VulnScanCheck struct {
	src      ports.VulnSource
	repoRoot RepoRootFunc
}

// NewVulnScanCheck constructs a VulnScanCheck bound to a VulnSource and a
// RepoRootFunc. Both collaborators are required; passing nil will cause Run to
// return an error on first invocation.
func NewVulnScanCheck(src ports.VulnSource, repoRoot RepoRootFunc) *VulnScanCheck {
	return &VulnScanCheck{src: src, repoRoot: repoRoot}
}

// Name returns the Prometheus / finding-rule attribution name.
func (c *VulnScanCheck) Name() string { return "vuln-scan" }

// Run scans the module dependency set against the advisory cache when go.mod
// is among the promotion's changed files. When go.mod was not touched it is a
// no-op returning (nil, nil).
func (c *VulnScanCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.src == nil || c.repoRoot == nil {
		return nil, fmt.Errorf("vuln-scan: nil dependency")
	}
	if !touchesGoMod(in.FilePaths) {
		return nil, nil
	}

	root, err := c.repoRoot(ctx, in.RepoID)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: resolve repo root: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: read go.mod: %w", err)
	}
	deps, err := manifest.ReadGoMod(content)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: parse go.mod: %w", err)
	}

	vulns, err := c.src.Scan(ctx, deps)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: scan: %w", err)
	}

	out := make([]*domain.Finding, 0, len(vulns))
	for _, v := range vulns {
		msg := fmt.Sprintf("%s: %s (affected range %s)", v.Package, v.Summary, v.AffectedRange)
		f, err := domain.NewFinding(
			in.RepoID, in.Branch,
			mapSeverity(v.Severity),
			domain.LayerSecurity,
			"vuln",
			msg,
			domain.WithFileAnchor("go.mod"),
			domain.WithFindingKey(v.AdvisoryID+v.Package),
		)
		if err != nil {
			// A malformed advisory should not abort the whole check; skip it.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// touchesGoMod reports whether any changed path is a go.mod. FilePaths is
// populated from PromotionBatch.Files[].Path, which (depending on the source)
// carries either a repo-root-relative path (git diff seam) or a full
// filesystem path (cold-scan walker). Matching by basename catches both. A
// nested vendor/.../go.mod would also trigger; that's acceptable because the
// scan itself only reads {repoRoot}/go.mod — at worst we run an extra scan
// against the root, never on the wrong manifest.
func touchesGoMod(paths []string) bool {
	for _, p := range paths {
		if filepath.Base(p) == "go.mod" {
			return true
		}
	}
	return false
}

// mapSeverity translates an OSV severity label onto the domain Severity enum.
// Unknown labels fall back to Medium so an advisory is never silently dropped
// for want of a recognised label.
func mapSeverity(label string) domain.Severity {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "CRITICAL":
		return domain.SeverityCritical
	case "HIGH":
		return domain.SeverityHigh
	case "MEDIUM", "MODERATE":
		return domain.SeverityMedium
	case "LOW":
		return domain.SeverityLow
	default:
		return domain.SeverityMedium
	}
}
