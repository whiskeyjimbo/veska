package checks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

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
// is among the promotion's changed files. When the repo has no go.mod at all
// it is a no-op returning (nil, nil) — no manifest, no scan.
//
// Historically this check skipped any promotion that didn't include go.mod in
// its file list, which collapsed two distinct cases into "no scan": (a) the
// commit really didn't touch go.mod, and (b) the promotion was a partial
// re-promote (e.g. `eng_promote_repo` invoked by `veska config reload`, which
// only restages the latest commit's changed files). Case (b) meant
// retroactive vuln scans after enabling [vuln_source] never fired on repos
// whose last commit was unrelated to deps — exactly the gap that hit during
// junior-journey round 3 (solov2-jtl5.4). The scan against the OSV cache is
// ~35ms; running it on every promote is cheap, and findings dedup by
// finding_key so a no-change re-scan is a no-op at the storage layer.
func (c *VulnScanCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.src == nil || c.repoRoot == nil {
		return nil, fmt.Errorf("vuln-scan: nil dependency")
	}

	root, err := c.repoRoot(ctx, in.RepoID)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: resolve repo root: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		if os.IsNotExist(err) {
			// No go.mod, nothing to scan. Not an error — non-Go repos are
			// a normal case once the touchesGoMod gate is gone.
			return nil, nil
		}
		return nil, fmt.Errorf("vuln-scan: read go.mod: %w", err)
	}
	deps, err := manifest.ReadGoMod(content)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: parse go.mod: %w", err)
	}
	// solov2-5dxw: build module->line map so the finding message points
	// at the offending require line for editor jump-to. A parse error here
	// would have failed manifest.ReadGoMod already; we ignore it and fall
	// back to omitting the line if it somehow slips through.
	lineFor := map[string]int{}
	if mf, perr := modfile.Parse("go.mod", content, nil); perr == nil {
		for _, r := range mf.Require {
			if r.Syntax != nil && r.Syntax.Start.Line > 0 {
				lineFor[r.Mod.Path] = r.Syntax.Start.Line
			}
		}
	}

	start := time.Now()
	vulns, err := c.src.Scan(ctx, deps)
	if err != nil {
		return nil, fmt.Errorf("vuln-scan: scan: %w", err)
	}

	out := make([]*domain.Finding, 0, len(vulns))
	for _, v := range vulns {
		// solov2-fr2a: lead the message with the advisory ID so triage
		// doesn't need to grep the OSV cache.
		// solov2-5dxw: include the offending go.mod line when known so
		// the message is editor-clickable (the findings table has no
		// line column today).
		loc := "go.mod"
		if ln, ok := lineFor[v.Package]; ok {
			loc = fmt.Sprintf("go.mod:%d", ln)
		}
		msg := fmt.Sprintf("%s [%s] %s: %s (affected range %s)", loc, v.AdvisoryID, v.Package, v.Summary, v.AffectedRange)
		// solov2-ka54: when the OSV adapter dedupes aliased advisories
		// (GHSA vs GO- vs CVE for the same vuln), cross-reference the
		// suppressed IDs so users can still grep their tracker.
		if len(v.Aliases) > 0 {
			msg += "; aliases: " + strings.Join(v.Aliases, ", ")
		}
		// solov2-gpvy: append a remediation hint so the finding is
		// directly actionable from `veska findings show` without a
		// trip to the advisory website.
		if v.FixedVersion != "" {
			msg += fmt.Sprintf("; fix: go get %s@%s", v.Package, v.FixedVersion)
		}
		f, err := domain.NewFinding(
			in.RepoID, in.Branch,
			mapSeverity(v.Severity),
			domain.LayerSecurity,
			"vulnerable_dependency",
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
	// solov2-fw6z: per-promotion log line so operators can confirm the
	// check ran for a given git_sha. The 'vulnrefresh' log lines only
	// reflect the OSV cache pull.
	slog.Info("vuln-scan: scanned",
		"repo_id", in.RepoID,
		"branch", in.Branch,
		"deps", len(deps),
		"findings", len(out),
		"elapsed_ms", time.Since(start).Milliseconds(),
	)
	return out, nil
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
