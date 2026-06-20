// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"context"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Named failing checks in SecurityVerdict.Failures. Stable strings: CI and
// agents match on them.
const (
	// FailNewSecretLeak: the candidate's added lines introduced a secret.
	FailNewSecretLeak = "new_secret_leak"
	// FailNewVulnDep: the candidate introduced a vulnerable dependency absent
	// at base.
	FailNewVulnDep = "new_vulnerable_dependency"
	// FailVulnUnchecked: vuln scanning is configured but could not run over the
	// candidate (manifest read / parse / scan error) - degraded, never a pass.
	// Distinct from "vuln not configured", which is not-applicable and silent.
	FailVulnUnchecked = "vuln_unchecked"
)

// Seams the SecurityGate consumes, wired to the real checks in the CLI and
// faked in tests. Keeping them as function types holds the VulnSource, the git
// ref reader, and the manifest parsers OUT of the application layer.
type (
	// SecretsScanFn scans a promotion-shaped Input's added lines and returns
	// secret_leak findings. Satisfied by checks.SecretsScanCheck.Run.
	SecretsScanFn func(ctx context.Context, in checks.Input) ([]*domain.Finding, error)

	// DepsScanFn matches a manifest's parsed deps against the advisory source
	// and returns vulnerable_dependency findings anchored on manifestPath.
	// Satisfied by a closure over checks.ScanManifestDeps bound to a VulnSource.
	DepsScanFn func(ctx context.Context, repoID, branch, manifestPath string, deps []ports.Dependency) ([]*domain.Finding, error)

	// ManifestReaderFn parses a manifest file's bytes into its dependency set.
	// The gate holds a registry of these keyed by manifest path (go.mod today;
	// package.json / requirements.txt drop in with no gate change once a
	// multi-ecosystem advisory source exists).
	ManifestReaderFn func(content []byte) ([]ports.Dependency, error)

	// RefContentFn reads a file's content at a git ref. present is false when
	// the file is absent at that ref (an added file has no base content); that
	// is not an error. Satisfied by a git.FileAtRef adapter.
	RefContentFn func(ctx context.Context, path, ref string) (content []byte, present bool, err error)
)

// SecurityFinding is one net-new security finding surfaced by the gate.
type SecurityFinding struct {
	Rule      string `json:"rule"`
	FindingID string `json:"finding_id"`
	Message   string `json:"message"`
}

// SecurityInput is the candidate change the gate judges.
type SecurityInput struct {
	RepoID     string
	Branch     string
	BaseRef    string
	CandRef    string
	AddedLines map[string][]checks.Line
	ReadAtRef  RefContentFn
}

// SecurityVerdict is the net-new security gate result. Pass is true only when
// no net-new secret was introduced AND vuln is either not-applicable or
// CHECKED-and-clean. An applicable-but-unchecked vuln dimension fails (fail-safe).
type SecurityVerdict struct {
	Pass           bool              `json:"pass"`
	NewSecretLeaks []SecurityFinding `json:"new_secret_leaks"`
	NewVulnDeps    []SecurityFinding `json:"new_vuln_deps"`
	// VulnApplicable is true when a vuln source is configured. When false the
	// vuln dimension is skipped entirely (mirrors the daemon registering
	// vuln-scan only when enabled) and never gates the verdict.
	VulnApplicable bool `json:"vuln_applicable"`
	// VulnChecked is meaningful only when VulnApplicable: false means the scan
	// could not run (degraded → FAIL).
	VulnChecked bool `json:"vuln_checked"`
}

// Failures returns the stable failing-check names.
func (v SecurityVerdict) Failures() []string {
	if v.Pass {
		return nil
	}
	var out []string
	if len(v.NewSecretLeaks) > 0 {
		out = append(out, FailNewSecretLeak)
	}
	if v.VulnApplicable && !v.VulnChecked {
		out = append(out, FailVulnUnchecked)
	}
	if v.VulnApplicable && v.VulnChecked && len(v.NewVulnDeps) > 0 {
		out = append(out, FailNewVulnDep)
	}
	return out
}

// ExitCode is the process exit code for CI gating: 0 on PASS, 1 on FAIL.
func (v SecurityVerdict) ExitCode() int {
	if v.Pass {
		return 0
	}
	return 1
}

// SecurityGate gates a candidate change on net-new security findings under two
// rules: secret_leak (added-line scan - language-agnostic) and
// vulnerable_dependency (manifest finding-delta by finding_id). It is a blanket
// gate: no target finding, no graph index - pure git refs + scanners.
type SecurityGate struct {
	scanSecrets SecretsScanFn
	scanDeps    DepsScanFn
	readers     map[string]ManifestReaderFn
	vulnEnabled bool
}

// NewSecurityGate constructs a SecurityGate. readers maps each recognized
// manifest path to its parser; vulnEnabled reflects whether an advisory source
// is configured (when false, the vuln dimension is not-applicable).
func NewSecurityGate(scanSecrets SecretsScanFn, scanDeps DepsScanFn, readers map[string]ManifestReaderFn, vulnEnabled bool) *SecurityGate {
	return &SecurityGate{scanSecrets: scanSecrets, scanDeps: scanDeps, readers: readers, vulnEnabled: vulnEnabled}
}

// Evaluate runs both dimensions over the candidate and returns the verdict.
func (g *SecurityGate) Evaluate(ctx context.Context, in SecurityInput) (SecurityVerdict, error) {
	v := SecurityVerdict{VulnApplicable: g.vulnEnabled}

	// secret_leak: scanning the candidate's added lines yields ONLY net-new
	// secrets - a new secret must land on an added/modified line, and a
	// pre-existing secret on an untouched line is never scanned. No base diff.
	secrets, err := g.scanSecrets(ctx, checks.Input{RepoID: in.RepoID, Branch: in.Branch, AddedLines: in.AddedLines})
	if err != nil {
		return SecurityVerdict{}, fmt.Errorf("security-gate: secrets scan: %w", err)
	}
	for _, f := range secrets {
		v.NewSecretLeaks = append(v.NewSecretLeaks, toSecurityFinding(f))
	}

	// vulnerable_dependency: scan each recognized manifest at base and
	// candidate refs; net-new = candidate findings whose finding_id is absent
	// at base. finding_id excludes the line number, so a dep that merely shifts
	// line is correctly not net-new.
	if g.vulnEnabled {
		checked := true
		var newVulns []SecurityFinding
		for path, reader := range g.readers {
			baseF, candF, derr := g.diffManifest(ctx, in, path, reader)
			if derr != nil {
				checked = false
				break
			}
			baseIDs := make(map[string]struct{}, len(baseF))
			for _, f := range baseF {
				baseIDs[f.FindingID] = struct{}{}
			}
			for _, f := range candF {
				if _, seen := baseIDs[f.FindingID]; !seen {
					newVulns = append(newVulns, toSecurityFinding(f))
				}
			}
		}
		v.VulnChecked = checked
		if checked {
			v.NewVulnDeps = newVulns
		}
	}

	sortFindings(v.NewSecretLeaks)
	sortFindings(v.NewVulnDeps)
	v.Pass = len(v.NewSecretLeaks) == 0 &&
		(!g.vulnEnabled || (v.VulnChecked && len(v.NewVulnDeps) == 0))
	return v, nil
}

// diffManifest scans manifest `path` at the base and candidate refs.
func (g *SecurityGate) diffManifest(ctx context.Context, in SecurityInput, path string, reader ManifestReaderFn) (base, cand []*domain.Finding, err error) {
	base, err = g.scanRef(ctx, in, path, in.BaseRef, reader)
	if err != nil {
		return nil, nil, err
	}
	cand, err = g.scanRef(ctx, in, path, in.CandRef, reader)
	if err != nil {
		return nil, nil, err
	}
	return base, cand, nil
}

// scanRef reads manifest `path` at `ref`, parses its deps, and scans them. An
// absent manifest at the ref yields no findings (e.g. a PR that adds go.mod has
// an empty base side, so every candidate vuln is net-new).
func (g *SecurityGate) scanRef(ctx context.Context, in SecurityInput, path, ref string, reader ManifestReaderFn) ([]*domain.Finding, error) {
	content, present, err := in.ReadAtRef(ctx, path, ref)
	if err != nil {
		return nil, err
	}
	if !present || len(content) == 0 {
		return nil, nil
	}
	deps, err := reader(content)
	if err != nil {
		return nil, err
	}
	return g.scanDeps(ctx, in.RepoID, in.Branch, path, deps)
}

func toSecurityFinding(f *domain.Finding) SecurityFinding {
	return SecurityFinding{Rule: f.Rule, FindingID: f.FindingID, Message: f.Message}
}

func sortFindings(fs []SecurityFinding) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].FindingID < fs[j].FindingID })
}
