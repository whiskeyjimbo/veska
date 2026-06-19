// SPDX-License-Identifier: AGPL-3.0-only

package osv_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
)

var _ ports.VulnSource = (*osv.Adapter)(nil)

// failingTransport implements an http.RoundTripper that explicitly fails all requests.
// This is used to verify that the scan operations run completely offline without triggering network I/O.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network access is forbidden in this test")
}

// xnetAdvisory is a fixture OSV advisory targeting golang.org/x/net below v0.17.0.
const xnetAdvisory = `{
  "id": "GO-2023-9999",
  "summary": "Example HTTP/2 vulnerability in x/net",
  "severity": [{"type": "CVSS_V3", "score": "7.5"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "golang.org/x/net"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "0.17.0"}]}]
  }]
}`

// textAdvisory is a fixture OSV advisory affecting the golang.org/x/text package.
const textAdvisory = `{
  "id": "GO-2024-0001",
  "summary": "Example issue in x/text",
  "severity": [{"type": "CVSS_V3", "score": "5.3"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "golang.org/x/text"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0.3.0"}, {"last_affected": "0.3.7"}]}]
  }]
}`

// fooV2Advisory is a fixture OSV advisory targeting example.com/foo v2 in the v2.0.0-v2.2.0 range.
const fooV2Advisory = `{
  "id": "GO-2024-0002",
  "summary": "Example issue in example.com/foo v2",
  "severity": [{"type": "CVSS_V3", "score": "6.1"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "example.com/foo"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "2.0.0"}, {"fixed": "2.2.0"}]}]
  }]
}`

// jwtGHSA and jwtGOAdvisory describe the same jwt-go auth-bypass vulnerability.
// They are used to verify that duplicate findings are collapsed with preference given to GHSA IDs.
const jwtGHSA = `{
  "id": "GHSA-w73w-5m7g-f7qc",
  "aliases": ["GO-2020-0017", "CVE-2020-26160"],
  "summary": "Authorization bypass in github.com/dgrijalva/jwt-go",
  "severity": [{"type": "CVSS_V3", "score": "7.1"}],
  "database_specific": {"severity": "HIGH"},
  "affected": [{
    "package": {"ecosystem": "Go", "name": "github.com/dgrijalva/jwt-go"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"last_affected": "3.2.0"}]}]
  }]
}`

const jwtGOAdvisory = `{
  "id": "GO-2020-0017",
  "aliases": ["GHSA-w73w-5m7g-f7qc", "CVE-2020-26160"],
  "summary": "Authorization bypass in github.com/dgrijalva/jwt-go",
  "severity": [{"type": "CVSS_V3", "score": "7.1"}],
  "database_specific": {"severity": "MODERATE"},
  "affected": [{
    "package": {"ecosystem": "Go", "name": "github.com/dgrijalva/jwt-go"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"last_affected": "3.2.0"}]}]
  }]
}`

func writeFixtureCache(t *testing.T, advisories map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range advisories {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

// TestScan_DedupesAliasedAdvisories verifies that multiple advisories describing the
// same vulnerability under different IDs are collapsed into a single finding.
func TestScan_DedupesAliasedAdvisories(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{
		"GHSA-w73w-5m7g-f7qc.json": jwtGHSA,
		"GO-2020-0017.json":        jwtGOAdvisory,
	})
	a := osv.New(osv.WithCacheDir(dir))
	got, err := a.Scan(context.Background(), []ports.Dependency{
		{Ecosystem: "Go", Name: "github.com/dgrijalva/jwt-go", Version: "v3.2.0+incompatible"},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 deduped finding, got %d: %v", len(got), got)
	}
	if got[0].AdvisoryID != "GHSA-w73w-5m7g-f7qc" {
		t.Errorf("canonical AdvisoryID = %q, want GHSA-w73w-5m7g-f7qc (GHSA-rank wins)", got[0].AdvisoryID)
	}
	wantAliases := []string{"CVE-2020-26160", "GO-2020-0017"}
	if len(got[0].Aliases) != len(wantAliases) {
		t.Fatalf("Aliases = %v, want %v", got[0].Aliases, wantAliases)
	}
	for i, a := range wantAliases {
		if got[0].Aliases[i] != a {
			t.Errorf("Aliases[%d] = %q, want %q", i, got[0].Aliases[i], a)
		}
	}
}

func TestScan_KnownVulnerableDepYieldsFinding(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{
		"GO-2023-9999.json": xnetAdvisory,
		"GO-2024-0001.json": textAdvisory,
	})
	a := osv.New(osv.WithCacheDir(dir))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.15.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %v", len(got), got)
	}
	f := got[0]
	if f.AdvisoryID != "GO-2023-9999" {
		t.Errorf("AdvisoryID: got %q, want GO-2023-9999", f.AdvisoryID)
	}
	if f.Package != "golang.org/x/net" {
		t.Errorf("Package: got %q", f.Package)
	}
	if f.Severity != "7.5" {
		t.Errorf("Severity: got %q, want 7.5", f.Severity)
	}
	if f.AffectedRange == "" || f.Summary == "" {
		t.Errorf("AffectedRange/Summary should be populated: %+v", f)
	}
}

// TestScan_PseudoVersionAndIncompatible verifies that pseudo-versions and "+incompatible"
// suffixes are parsed correctly and matched against advisory ranges without being discarded.
func TestScan_PseudoVersionAndIncompatible(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{
		"GO-2023-9999.json": xnetAdvisory,
		"GO-2024-0002.json": fooV2Advisory,
	})
	a := osv.New(osv.WithCacheDir(dir))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.16.1-0.20240115120000-abcdef123456"},
		{Ecosystem: "Go", Name: "example.com/foo", Version: "v2.1.0+incompatible"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 findings (pseudo-version + incompatible), got %d: %+v", len(got), got)
	}
	byPkg := map[string]bool{}
	for _, f := range got {
		byPkg[f.Package] = true
	}
	if !byPkg["golang.org/x/net"] {
		t.Error("pseudo-version dependency was not matched")
	}
	if !byPkg["example.com/foo"] {
		t.Error("+incompatible dependency was not matched")
	}
}

func TestScan_CleanDepYieldsNoFinding(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{"GO-2023-9999.json": xnetAdvisory})
	a := osv.New(osv.WithCacheDir(dir))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.17.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no findings, got %v", got)
	}
}

func TestScan_UnrelatedDepYieldsNoFinding(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{"GO-2023-9999.json": xnetAdvisory})
	a := osv.New(osv.WithCacheDir(dir))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "github.com/some/other", Version: "v1.0.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no findings, got %v", got)
	}
}

func TestScan_LastAffectedRangeBoundary(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{"GO-2024-0001.json": textAdvisory})
	a := osv.New(osv.WithCacheDir(dir))

	cases := []struct {
		version string
		want    bool
	}{
		{"v0.3.7", true},
		{"v0.3.8", false},
		{"v0.2.0", false},
	}
	for _, tc := range cases {
		deps := []ports.Dependency{{Ecosystem: "Go", Name: "golang.org/x/text", Version: tc.version}}
		got, err := a.Scan(context.Background(), deps)
		if err != nil {
			t.Fatalf("Scan(%s): %v", tc.version, err)
		}
		if (len(got) > 0) != tc.want {
			t.Errorf("version %s: got %d findings, want affected=%v", tc.version, len(got), tc.want)
		}
	}
}

func TestScan_MissingCacheReturnsNilNil(t *testing.T) {
	t.Parallel()
	a := osv.New(osv.WithCacheDir(filepath.Join(t.TempDir(), "does-not-exist")))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.15.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("expected no error for missing cache, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings for missing cache, got %v", got)
	}
}

func TestScan_EmptyCacheReturnsNilNil(t *testing.T) {
	t.Parallel()
	a := osv.New(osv.WithCacheDir(t.TempDir()))

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.15.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings for empty cache, got %v", got)
	}
}

// TestScan_IsOffline verifies that Scan runs successfully offline without triggering HTTP requests.
func TestScan_IsOffline(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{"GO-2023-9999.json": xnetAdvisory})
	a := osv.New(
		osv.WithCacheDir(dir),
		osv.WithHTTPClient(&http.Client{Transport: failingTransport{}}),
	)

	deps := []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.15.0"},
	}
	got, err := a.Scan(context.Background(), deps)
	if err != nil {
		t.Fatalf("Scan must succeed offline, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 finding offline, got %v", got)
	}
}

func fixtureZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestRefresh_DownloadsAndExtractsDump(t *testing.T) {
	t.Parallel()
	zipData := fixtureZip(t, map[string]string{
		"GO-2023-9999.json": xnetAdvisory,
		"GO-2024-0001.json": textAdvisory,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipData)
	}))
	defer srv.Close()

	cacheDir := filepath.Join(t.TempDir(), "osv")
	a := osv.New(osv.WithCacheDir(cacheDir), osv.WithDumpURL(srv.URL))

	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	for _, name := range []string{"GO-2023-9999.json", "GO-2024-0001.json"} {
		if _, err := os.Stat(filepath.Join(cacheDir, name)); err != nil {
			t.Errorf("expected %s in cache: %v", name, err)
		}
	}

	got, err := a.Scan(context.Background(), []ports.Dependency{
		{Ecosystem: "Go", Name: "golang.org/x/net", Version: "v0.15.0"},
	})
	if err != nil {
		t.Fatalf("Scan after Refresh: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 finding after Refresh, got %v", got)
	}
}

func TestRefresh_Non200ReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	a := osv.New(osv.WithCacheDir(t.TempDir()), osv.WithDumpURL(srv.URL))
	if err := a.Refresh(context.Background()); err == nil {
		t.Fatal("expected error on non-200 response")
		return
	}
}
