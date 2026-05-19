// Package osv provides a ports.VulnSource implementation backed by the OSV.dev
// advisory database.
//
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

	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	// dumpURL is the OSV full Go-ecosystem advisory dump.
	dumpURL = "https://osv-vulnerabilities.storage.googleapis.com/Go/all.zip"

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
		dumpURL:  dumpURL,
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
	ID       string        `json:"id"`
	Summary  string        `json:"summary"`
	Affected []osvAffected `json:"affected"`
	Severity []osvSeverity `json:"severity"`
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
	return findings, nil
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
				Package:       aff.Package.Name,
				AffectedRange: rangeString(aff.Ranges),
				Severity:      severityLabel(adv.Severity),
				Summary:       adv.Summary,
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

// severityLabel extracts a human-readable severity from the OSV severity list,
// preferring a CVSS score if present.
func severityLabel(sev []osvSeverity) string {
	for _, s := range sev {
		if s.Score != "" {
			return s.Score
		}
	}
	return ""
}
