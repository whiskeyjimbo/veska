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

// Compile-time interface satisfaction check.
var _ ports.VulnSource = (*osv.Adapter)(nil)

// failingTransport fails any HTTP round-trip. An adapter built with it proves a
// code path performs no network I/O when it still succeeds.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network access is forbidden in this test")
}

// xnetAdvisory is a fixture OSV advisory: golang.org/x/net affected below
// v0.17.0.
const xnetAdvisory = `{
  "id": "GO-2023-9999",
  "summary": "Example HTTP/2 vulnerability in x/net",
  "severity": [{"type": "CVSS_V3", "score": "7.5"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "golang.org/x/net"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "0.17.0"}]}]
  }]
}`

// textAdvisory is a fixture OSV advisory affecting an unrelated package.
const textAdvisory = `{
  "id": "GO-2024-0001",
  "summary": "Example issue in x/text",
  "severity": [{"type": "CVSS_V3", "score": "5.3"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "golang.org/x/text"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0.3.0"}, {"last_affected": "0.3.7"}]}]
  }]
}`

// writeFixtureCache builds an OSV cache directory containing the given advisory
// JSON documents, keyed by filename.
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

func TestScan_CleanDepYieldsNoFinding(t *testing.T) {
	t.Parallel()
	dir := writeFixtureCache(t, map[string]string{"GO-2023-9999.json": xnetAdvisory})
	a := osv.New(osv.WithCacheDir(dir))

	// v0.17.0 is the fixed version — not affected.
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
		{"v0.3.7", true},  // inclusive last_affected
		{"v0.3.8", false}, // past last_affected
		{"v0.2.0", false}, // before introduced
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
	a := osv.New(osv.WithCacheDir(t.TempDir())) // exists but empty

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

// TestScan_IsOffline proves Scan performs no network I/O: the adapter is built
// with a transport that fails any request, yet Scan still produces findings.
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

// fixtureZip builds an in-memory zip of OSV advisory JSON files.
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

	// The freshly-refreshed cache must be scannable.
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
