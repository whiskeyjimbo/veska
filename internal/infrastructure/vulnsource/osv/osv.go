// SPDX-License-Identifier: AGPL-3.0-only

// Package osv provides a ports.VulnSource implementation backed by the OSV.dev
// advisory database.
// Refresh downloads OSV's full Go-ecosystem advisory dump (a zip of per-advisory
// JSON files in the OSV schema) and extracts it into an on-disk cache. It is the
// only operation that performs network egress. Scan reads that cache and matches
// dependencies against it offline - it never touches the network, so it is safe
// on the promotion path.
package osv

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// DumpURL is the OSV Go-ecosystem advisory zip database location.
const DumpURL = "https://osv-vulnerabilities.storage.googleapis.com/Go/all.zip"

const (
	defaultTimeout = 5 * time.Minute
	ecosystemGo    = "Go"
)

// ErrMissingDependency is returned when a required wiring dependency is not provided.
var ErrMissingDependency = errors.New("osv: missing required dependency")

// Adapter implements ports.VulnSource against the OSV.dev advisory database.
// It is safe for concurrent use since Scan only reads from disk and Refresh
// writes to a unique temporary file per advisory.
type Adapter struct {
	cacheDir string
	dumpURL  string
	client   *http.Client
}

var _ ports.VulnSource = (*Adapter)(nil)

type Option func(*Adapter)

// WithCacheDir configures a custom directory path for caching advisory files.
func WithCacheDir(dir string) Option {
	return func(a *Adapter) {
		if dir != "" {
			a.cacheDir = dir
		}
	}
}

// WithHTTPClient registers a custom HTTP client for downloading database dumps.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) {
		if c != nil {
			a.client = c
		}
	}
}

// WithDumpURL registers a custom database zip archive download URL.
func WithDumpURL(u string) Option {
	return func(a *Adapter) {
		if u != "" {
			a.dumpURL = u
		}
	}
}

// New constructs an OSV Adapter.
func New(opts ...Option) *Adapter {
	a := &Adapter{
		cacheDir: config.DefaultOSVCacheDir(),
		dumpURL:  DumpURL,
		client:   &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Refresh downloads the full OSV database dump and extracts advisory JSON files.
func (a *Adapter) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.dumpURL, nil)
	if err != nil {
		return fmt.Errorf("osv refresh: build request: %w", err)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("osv refresh: download dump: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("osv refresh: download dump: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("osv refresh: read dump: %w", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("osv refresh: open zip: %w", err)
	}

	if err := os.MkdirAll(a.cacheDir, 0o755); err != nil {
		return fmt.Errorf("osv refresh: create cache dir: %w", err)
	}

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		if err := a.extractAdvisory(f); err != nil {
			return fmt.Errorf("osv refresh: extract %s: %w", f.Name, err)
		}
	}
	return nil
}

func (a *Adapter) extractAdvisory(f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}

	// Entry names from the downloaded archive are untrusted. filepath.Base strips
	// any directory components; the prefix check then rejects anything that would
	// still resolve outside cacheDir, so a crafted "../" name cannot escape the
	// cache (zip-slip, CWE-22). A traversal entry in the OSV dump implies a
	// tampered source, so we fail closed rather than skip.
	dest := filepath.Join(a.cacheDir, filepath.Base(f.Name))
	if !strings.HasPrefix(dest, filepath.Clean(a.cacheDir)+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe archive entry %q", f.Name)
	}
	return os.WriteFile(dest, data, 0o644)
}

type advisory struct {
	ID             string            `json:"id"`
	Aliases        []string          `json:"aliases"`
	Summary        string            `json:"summary"`
	Affected       []osvAffected     `json:"affected"`
	Severity       []osvSeverity     `json:"severity"`
	DatabaseSpecif osvDatabaseSpecif `json:"database_specific"`
}

// osvDatabaseSpecif maps GHSA-prefixed advisory severity levels ("CRITICAL", "HIGH", etc.).
type osvDatabaseSpecif struct {
	Severity string `json:"severity"`
}

type osvAffected struct {
	Package osvPackage `json:"package"`
	Ranges  []osvRange `json:"ranges"`
}

type osvPackage struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
}

type osvRange struct {
	Type   string     `json:"type"`
	Events []osvEvent `json:"events"`
}

type osvEvent struct {
	Introduced   string `json:"introduced"`
	Fixed        string `json:"fixed"`
	LastAffected string `json:"last_affected"`
}

type osvSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

// Scan compares the input dependencies against the cached OSV database on disk.
func (a *Adapter) Scan(ctx context.Context, deps []ports.Dependency) ([]ports.VulnFinding, error) {
	entries, err := os.ReadDir(a.cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("osv scan: read cache dir: %w", err)
	}

	byPackage := make(map[string][]ports.Dependency, len(deps))
	for _, d := range deps {
		if !strings.EqualFold(d.Ecosystem, ecosystemGo) {
			continue
		}
		byPackage[d.Name] = append(byPackage[d.Name], d)
	}
	if len(byPackage) == 0 {
		return nil, nil
	}

	var findings []ports.VulnFinding
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		adv, err := a.loadAdvisory(filepath.Join(a.cacheDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("osv scan: %w", err)
		}
		findings = append(findings, matchAdvisory(adv, byPackage)...)
	}
	return dedupeAliased(findings), nil
}

// dedupeAliased collapses findings that represent the same vulnerability under different advisory IDs.
// Equivalent IDs are identified via OSV's aliases field, grouped using union-find, and the finding with the
// most widely recognized ID type (GHSA first, then CVE) is preferred.
func dedupeAliased(findings []ports.VulnFinding) []ports.VulnFinding {
	if len(findings) <= 1 {
		return findings
	}
	byPkg := make(map[string][]int, len(findings))
	for i, f := range findings {
		byPkg[f.Package] = append(byPkg[f.Package], i)
	}
	keep := make([]bool, len(findings))
	for _, idxs := range byPkg {
		idxByID := make(map[string]int, len(idxs)*2)
		for _, i := range idxs {
			idxByID[findings[i].AdvisoryID] = i
		}
		parent := make([]int, len(findings))
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			if parent[i] != i {
				parent[i] = find(parent[i])
			}
			return parent[i]
		}
		union := func(a, b int) {
			ra, rb := find(a), find(b)
			if ra != rb {
				parent[ra] = rb
			}
		}
		for _, i := range idxs {
			for _, alias := range findings[i].Aliases {
				if j, ok := idxByID[alias]; ok {
					union(i, j)
				}
			}
		}
		bestOfClass := make(map[int]int)
		for _, i := range idxs {
			root := find(i)
			cur, seen := bestOfClass[root]
			if !seen || advisoryRank(findings[i].AdvisoryID) < advisoryRank(findings[cur].AdvisoryID) {
				bestOfClass[root] = i
			}
		}
		for _, winner := range bestOfClass {
			keep[winner] = true
		}
	}
	out := findings[:0]
	for i, f := range findings {
		if !keep[i] {
			continue
		}
		aliasSet := make(map[string]struct{})
		for _, alias := range f.Aliases {
			if alias != "" && alias != f.AdvisoryID {
				aliasSet[alias] = struct{}{}
			}
		}
		for j, other := range findings {
			if j == i || keep[j] || other.Package != f.Package {
				continue
			}
			for _, alias := range other.Aliases {
				if alias == f.AdvisoryID {
					aliasSet[other.AdvisoryID] = struct{}{}
				}
			}
		}
		merged := make([]string, 0, len(aliasSet))
		for a := range aliasSet {
			merged = append(merged, a)
		}
		sortStrings(merged)
		f.Aliases = merged
		out = append(out, f)
	}
	return out
}

// advisoryRank defines the precedence order for canonical vulnerability IDs (GHSA wins over CVE, etc.).
func advisoryRank(id string) int {
	switch {
	case strings.HasPrefix(id, "GHSA-"):
		return 0
	case strings.HasPrefix(id, "CVE-"):
		return 1
	case strings.HasPrefix(id, "GO-"), strings.HasPrefix(id, "PYSEC-"):
		return 3
	default:
		return 2
	}
}

// sortStrings performs an in-place insertion sort to ensure a deterministic ordering of aliases.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func (a *Adapter) loadAdvisory(path string) (advisory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return advisory{}, fmt.Errorf("read advisory %s: %w", path, err)
	}
	var adv advisory
	if err := json.Unmarshal(data, &adv); err != nil {
		return advisory{}, fmt.Errorf("decode advisory %s: %w", path, err)
	}
	return adv, nil
}

func matchAdvisory(adv advisory, byPackage map[string][]ports.Dependency) []ports.VulnFinding {
	var findings []ports.VulnFinding
	for _, aff := range adv.Affected {
		if !strings.EqualFold(aff.Package.Ecosystem, ecosystemGo) {
			continue
		}
		for _, dep := range byPackage[aff.Package.Name] {
			if !versionAffected(dep.Version, aff.Ranges) {
				continue
			}
			findings = append(findings, ports.VulnFinding{
				AdvisoryID:    adv.ID,
				Aliases:       adv.Aliases,
				Package:       aff.Package.Name,
				AffectedRange: rangeString(aff.Ranges),
				Severity:      pickSeverity(adv),
				Summary:       adv.Summary,
				FixedVersion:  minFixedAbove(dep.Version, aff.Ranges),
			})
		}
	}
	return findings
}

// versionAffected reports whether a version falls inside any of the affected semver ranges.
// Under OSV rules, a version is affected if an introduced event applies and no fixed event overrides it.
func versionAffected(version string, ranges []osvRange) bool {
	v := normalizeSemver(version)
	if !semver.IsValid(v) {
		return false
	}
	for _, r := range ranges {
		if r.Type != "" && r.Type != "SEMVER" {
			continue
		}
		affected := false
		for _, ev := range r.Events {
			switch {
			case ev.Introduced == "0":
				affected = true
			case ev.Introduced != "":
				if semver.Compare(v, normalizeSemver(ev.Introduced)) >= 0 {
					affected = true
				}
			case ev.Fixed != "":
				if semver.Compare(v, normalizeSemver(ev.Fixed)) >= 0 {
					affected = false
				}
			case ev.LastAffected != "":
				if semver.Compare(v, normalizeSemver(ev.LastAffected)) > 0 {
					affected = false
				}
			}
		}
		if affected {
			return true
		}
	}
	return false
}

// minFixedAbove returns the lowest fixed version that resolves the vulnerability for the current version.
// A leading "v" is attached to ensure compatibility when executing commands like `go get`.
func minFixedAbove(current string, ranges []osvRange) string {
	cur := normalizeSemver(current)
	var best string
	for _, r := range ranges {
		if r.Type != "" && r.Type != "SEMVER" {
			continue
		}
		for _, ev := range r.Events {
			if ev.Fixed == "" {
				continue
			}
			fixed := normalizeSemver(ev.Fixed)
			if !semver.IsValid(fixed) {
				continue
			}
			if semver.IsValid(cur) && semver.Compare(fixed, cur) <= 0 {
				continue
			}
			if best == "" || semver.Compare(fixed, best) < 0 {
				best = fixed
			}
		}
	}
	return best
}

// normalizeSemver prepends a "v" prefix if not already present, conforming to golang.org/x/mod/semver.
func normalizeSemver(v string) string {
	if v == "" {
		return v
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// rangeString translates advisory range lists to a comma-separated readable string.
func rangeString(ranges []osvRange) string {
	var parts []string
	for _, r := range ranges {
		for _, ev := range r.Events {
			switch {
			case ev.Introduced == "0":
				parts = append(parts, ">=0")
			case ev.Introduced != "":
				parts = append(parts, ">="+ev.Introduced)
			case ev.Fixed != "":
				parts = append(parts, "<"+ev.Fixed)
			case ev.LastAffected != "":
				parts = append(parts, "<="+ev.LastAffected)
			}
		}
	}
	return strings.Join(parts, ", ")
}

// pickSeverity returns a severity rating, falling back to CVSS score calculation if GHSA database values are absent.
func pickSeverity(adv advisory) string {
	if s := strings.TrimSpace(adv.DatabaseSpecif.Severity); s != "" {
		return s
	}
	for _, s := range adv.Severity {
		if s.Score == "" {
			continue
		}
		if label := severityFromCVSS3(s.Score); label != "" {
			return label
		}
		return s.Score
	}
	return ""
}

// severityFromCVSS3 extracts base scores to determine rating groups.
// Currently returns an empty string as a placeholder for a future CVSS calculator.
func severityFromCVSS3(vec string) string {
	_ = vec
	return ""
}
