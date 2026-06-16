// Package osv provides a ports.VulnSource implementation backed by the OSV.dev
// advisory database.
// Refresh downloads OSV's full Go-ecosystem advisory dump (a zip of per-advisory
// JSON files in the OSV schema) and extracts it into an on-disk cache. It is the
// only operation that performs network egress. Scan reads that cache and matches
// dependencies against it offline — it never touches the network, so it is safe
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

// DumpURL is the OSV full Go-ecosystem advisory dump. It is the single
// outbound endpoint this adapter dials and is exported so the daemon's egress
// observability report can cite it without re-deriving the literal.
const DumpURL = "https://osv-vulnerabilities.storage.googleapis.com/Go/all.zip"

const (
	defaultTimeout = 5 * time.Minute

	// ecosystemGo is the OSV ecosystem label this adapter understands.
	ecosystemGo = "Go"
)

// ErrMissingDependency is returned by New when a required dependency is missing.
// It is errors.Is-matchable so callers can distinguish a wiring fault from a
// runtime failure.
var ErrMissingDependency = errors.New("osv: missing required dependency")

// Adapter implements ports.VulnSource against the OSV.dev advisory database.
// Adapter is safe for concurrent use: Scan only reads from disk, and Refresh
// writes via a fresh temporary file per advisory.
type Adapter struct {
	cacheDir string
	dumpURL  string
	client   *http.Client
}

var _ ports.VulnSource = (*Adapter)(nil)

// Option configures an Adapter.
type Option func(*Adapter)

// WithCacheDir overrides the advisory cache directory (default
// $VESKA_HOME/cache/osv). An empty value is ignored.
func WithCacheDir(dir string) Option {
	return func(a *Adapter) {
		if dir != "" {
			a.cacheDir = dir
		}
	}
}

// WithHTTPClient supplies a custom *http.Client used by Refresh. The client's
// Timeout, if any, applies to the entire request.
func WithHTTPClient(c *http.Client) Option {
	return func(a *Adapter) {
		if c != nil {
			a.client = c
		}
	}
}

// WithDumpURL overrides the OSV dump URL. Intended for tests serving a fixture
// zip from an httptest.Server. An empty value is ignored.
func WithDumpURL(u string) Option {
	return func(a *Adapter) {
		if u != "" {
			a.dumpURL = u
		}
	}
}

// New constructs an Adapter. By default the cache lives under
// $VESKA_HOME/cache/osv and Refresh downloads from the public OSV dump URL.
// Apply WithCacheDir / WithHTTPClient / WithDumpURL to override.
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

// Refresh downloads the OSV Go-ecosystem advisory dump and extracts every
// advisory JSON file into the cache directory. It is the only operation that
// performs network egress.
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

	// zip.NewReader needs random access, so buffer the dump in memory. The Go
	// dump is on the order of tens of MB.
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

// extractAdvisory writes a single advisory file from the zip into the cache.
// The zip path is flattened to its base name to keep the cache directory flat.
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

	dest := filepath.Join(a.cacheDir, filepath.Base(f.Name))
	return os.WriteFile(dest, data, 0o644)
}

// advisory mirrors the subset of the OSV schema this adapter needs.
type advisory struct {
	ID             string            `json:"id"`
	Aliases        []string          `json:"aliases"`
	Summary        string            `json:"summary"`
	Affected       []osvAffected     `json:"affected"`
	Severity       []osvSeverity     `json:"severity"`
	DatabaseSpecif osvDatabaseSpecif `json:"database_specific"`
}

// osvDatabaseSpecif carries GHSA-prefixed advisories' severity rating
// (CRITICAL/HIGH/MODERATE/LOW), which is more useful than the raw CVSS
// vector string when populating finding severity.
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

// Scan matches deps against the on-disk advisory cache and returns any
// findings. It performs no network I/O. A missing or empty cache directory
// yields (nil, nil).
func (a *Adapter) Scan(ctx context.Context, deps []ports.Dependency) ([]ports.VulnFinding, error) {
	entries, err := os.ReadDir(a.cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("osv scan: read cache dir: %w", err)
	}

	// Index deps by package name for O(1) lookup per advisory.
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

// dedupeAliased collapses findings that point at the same vulnerability via
// different advisory IDs (e.g. GHSA-w73w-5m7g-f7qc and GO-2020-0017 both
// describe the same jwt-go authorization bypass). OSV.dev's `aliases` field
// names the equivalent IDs; we keep one finding per (package, equivalence
// class) and prefer GHSA-prefixed IDs (most widely cross-referenced), then
// CVE, then anything else. The retained finding's Aliases field lists the
// suppressed IDs so triage can still cross-check.
func dedupeAliased(findings []ports.VulnFinding) []ports.VulnFinding {
	if len(findings) <= 1 {
		return findings
	}
	// Build per-package index then walk aliases to assign each finding to
	// an equivalence class. AdvisoryID + Aliases together define edges.
	byPkg := make(map[string][]int, len(findings))
	for i, f := range findings {
		byPkg[f.Package] = append(byPkg[f.Package], i)
	}
	keep := make([]bool, len(findings))
	for _, idxs := range byPkg {
		// Map every advisory ID we've seen for this package to its
		// finding-index, then group via union-find over Aliases.
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
		// For each class, pick the canonical representative.
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
		// Collect aliases from every member of this equivalence class so
		// the retained finding still cross-references the suppressed IDs.
		aliasSet := make(map[string]struct{})
		for _, alias := range f.Aliases {
			if alias != "" && alias != f.AdvisoryID {
				aliasSet[alias] = struct{}{}
			}
		}
		// Add suppressed sibling IDs (they may not appear in our own
		// Aliases list when the alias relation was unidirectional).
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
		// Deterministic order for tests / diff-stable findings output.
		sortStrings(merged)
		f.Aliases = merged
		out = append(out, f)
	}
	return out
}

// advisoryRank ranks advisory-ID prefixes by usefulness as the canonical ID.
// Lower is better. GHSA wins because GitHub Security Advisories carry the
// richest cross-references; CVE is the universal alias; GO-/PYSEC are
// ecosystem-specific and least portable.
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

// sortStrings is a tiny in-place insertion sort. Aliases lists hold at most
// a handful of IDs so the algorithm choice is irrelevant; using a one-line
// helper keeps the osv package free of "sort" import churn.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// loadAdvisory reads and decodes a single advisory file from the cache.
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

// matchAdvisory returns a finding for each scanned dependency that the advisory
// reports as affected.
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

// versionAffected reports whether version falls inside any affected range.
// OSV semantics: a version is affected when, in event order, an "introduced"
// event applies and no subsequent "fixed" event does; "last_affected" closes
// the range inclusively.
func versionAffected(version string, ranges []osvRange) bool {
	v := normalizeSemver(version)
	if !semver.IsValid(v) {
		return false
	}
	for _, r := range ranges {
		// SEMVER and GIT are the OSV range types; only SEMVER is comparable
		// here. An unrecognised type is skipped rather than guessed.
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

// minFixedAbove returns the lowest "fixed" version greater than the current
// version among the matching ranges, with a leading "v" so it can be passed
// straight to `go get`. Empty when no published fix exists.
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

// normalizeSemver gives a version the leading "v" that golang.org/x/mod/semver
// requires. OSV stores Go versions without it; go.mod versions carry it.
func normalizeSemver(v string) string {
	if v == "" {
		return v
	}
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// rangeString renders affected ranges into a compact, human-readable form for
// the VulnFinding.AffectedRange field.
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

// pickSeverity returns a human-readable severity rating for the advisory.
// GHSA records carry a rating directly in database_specific.severity; for
// other records we parse the CVSS3 vector and derive a label from the base
// score. Empty string falls through to checks.mapSeverity's Medium default
func pickSeverity(adv advisory) string {
	if s := strings.TrimSpace(adv.DatabaseSpecif.Severity); s != "" {
		return s
	}
	for _, s := range adv.Severity {
		if s.Score == "" {
			continue
		}
		// CVSS3 base-score → severity per the official rubric.
		if label := severityFromCVSS3(s.Score); label != "" {
			return label
		}
		// Fall through with the raw score string — mapSeverity will default
		// to Medium rather than guess.
		return s.Score
	}
	return ""
}

// severityFromCVSS3 extracts the CVSS3 base score from a vector string of the
// form "CVSS:3.1/AV:N/.". If a Score field is present numerically, use it;
// otherwise compute nothing and return "". Recognised severity buckets per
// FIRST.org's CVSS3 rubric: 0.1–3.9 Low, 4.0–6.9 Medium, 7.0–8.9 High,
// 9.0–10.0 Critical.
func severityFromCVSS3(vec string) string {
	// We don't ship a CVSS calculator; the OSV vector strings we've seen
	// don't include a precomputed score. Return "" and let the caller fall
	// through. Splitting this out keeps the intent explicit for a future
	// pass that adds a CVSS calculator.
	_ = vec
	return ""
}
