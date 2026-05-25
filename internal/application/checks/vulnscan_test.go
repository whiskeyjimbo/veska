package checks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeVulnSource is a deterministic VulnSource stub for unit tests.
type fakeVulnSource struct {
	findings []ports.VulnFinding
	err      error
	scanned  []ports.Dependency
}

func (f *fakeVulnSource) Refresh(ctx context.Context) error { return nil }

func (f *fakeVulnSource) Scan(ctx context.Context, deps []ports.Dependency) ([]ports.VulnFinding, error) {
	f.scanned = deps
	if f.err != nil {
		return nil, f.err
	}
	return f.findings, nil
}

const goModFixture = `module example.com/app

go 1.22

require github.com/vulnerable/pkg v1.0.0
`

// writeGoMod creates a temp dir with a go.mod fixture and returns a
// RepoRootFunc resolving any repoID to that dir.
func writeGoMod(t *testing.T, content string) RepoRootFunc {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return func(ctx context.Context, repoID string) (string, error) {
		return dir, nil
	}
}

func TestVulnScanCheck_GoModTouched_EmitsFindings(t *testing.T) {
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-aaaa-bbbb-cccc", Package: "github.com/vulnerable/pkg",
			AffectedRange: "<1.2.0", Severity: "HIGH", Summary: "remote code execution"},
		{AdvisoryID: "CVE-2024-9999", Package: "github.com/vulnerable/pkg",
			AffectedRange: "<2.0.0", Severity: "CRITICAL", Summary: "auth bypass"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goModFixture))

	in := Input{RepoID: "repo1", Branch: "main", FilePaths: []string{"go.mod", "main.go"}}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 findings, got %d", len(got))
	}
	for _, f := range got {
		if f.SourceLayer != domain.LayerSecurity {
			t.Errorf("source_layer = %q, want security", f.SourceLayer)
		}
		if f.Rule != "vulnerable_dependency" {
			t.Errorf("rule = %q, want vulnerable_dependency", f.Rule)
		}
		if f.FilePath == nil || *f.FilePath != "go.mod" {
			t.Errorf("file anchor = %v, want go.mod", f.FilePath)
		}
	}
	if got[0].Severity != domain.SeverityHigh {
		t.Errorf("severity = %q, want high", got[0].Severity)
	}
	if got[1].Severity != domain.SeverityCritical {
		t.Errorf("severity = %q, want critical", got[1].Severity)
	}
	if len(src.scanned) != 1 || src.scanned[0].Name != "github.com/vulnerable/pkg" {
		t.Errorf("scanned deps = %v", src.scanned)
	}
}

// TestVulnScanCheck_MessageCarriesGoModLine pins solov2-5dxw: the
// finding message must include the go.mod line of the offending require
// so editors can jump to source. (The findings table has no dedicated
// line column today — see also the schema note in the issue.)
func TestVulnScanCheck_MessageCarriesGoModLine(t *testing.T) {
	const goMod = `module example.com/app

go 1.22

require (
	github.com/innocent/pkg v1.0.0
	github.com/vulnerable/pkg v1.0.0
)
`
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-x", Package: "github.com/vulnerable/pkg",
			AffectedRange: "<2.0.0", Severity: "HIGH", Summary: "rce"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goMod))
	got, err := c.Run(context.Background(), Input{RepoID: "r", Branch: "main", FilePaths: []string{"go.mod"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d", len(got))
	}
	// vulnerable/pkg is on line 7 of the fixture (require keyword on 5,
	// innocent on 6, vulnerable on 7).
	const wantPrefix = "go.mod:7 "
	if !startsWith(got[0].Message, wantPrefix) {
		t.Errorf("message=%q; want prefix %q", got[0].Message, wantPrefix)
	}
}

func startsWith(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

func TestVulnScanCheck_GoModNotTouched_NoFindings(t *testing.T) {
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-aaaa-bbbb-cccc", Package: "p", Severity: "HIGH"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goModFixture))

	in := Input{RepoID: "repo1", Branch: "main", FilePaths: []string{"main.go", "README.md"}}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no findings, got %d", len(got))
	}
}

func TestVulnScanCheck_Idempotent(t *testing.T) {
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-aaaa-bbbb-cccc", Package: "github.com/vulnerable/pkg",
			AffectedRange: "<1.2.0", Severity: "HIGH", Summary: "rce"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goModFixture))
	in := Input{RepoID: "repo1", Branch: "main", FilePaths: []string{"go.mod"}}

	first, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	second, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want 1 finding each run, got %d / %d", len(first), len(second))
	}
	if first[0].FindingID != second[0].FindingID {
		t.Errorf("finding_id not stable: %q != %q", first[0].FindingID, second[0].FindingID)
	}
}

func TestVulnScanCheck_NilSource(t *testing.T) {
	c := NewVulnScanCheck(nil, writeGoMod(t, goModFixture))
	if _, err := c.Run(context.Background(), Input{FilePaths: []string{"go.mod"}}); err == nil {
		t.Fatal("want error on nil source")
		return
	}
}

func TestVulnScanCheck_GoModReadFailure(t *testing.T) {
	root := func(ctx context.Context, repoID string) (string, error) {
		return t.TempDir(), nil // no go.mod written
	}
	c := NewVulnScanCheck(&fakeVulnSource{}, root)
	if _, err := c.Run(context.Background(), Input{FilePaths: []string{"go.mod"}}); err == nil {
		t.Fatal("want error on missing go.mod")
		return
	}
}

// TestVulnScanCheck_GoModTouched_AbsolutePath is the regression for
// solov2-3tqb: the cold-scan / fsnotify Save path passes full filesystem
// paths through to PromotionBatch.Files[].Path, so the gate must match
// "/abs/path/to/repo/go.mod" the same way it matches "go.mod".
func TestVulnScanCheck_GoModTouched_AbsolutePath(t *testing.T) {
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-aaaa-bbbb-cccc", Package: "github.com/vulnerable/pkg",
			AffectedRange: "<1.2.0", Severity: "HIGH", Summary: "rce"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goModFixture))

	in := Input{RepoID: "repo1", Branch: "main", FilePaths: []string{
		"/home/example/src/myrepo/go.mod",
		"/home/example/src/myrepo/main.go",
	}}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding from absolute-path go.mod, got %d", len(got))
	}
}

func TestVulnScanCheck_Name(t *testing.T) {
	c := NewVulnScanCheck(&fakeVulnSource{}, writeGoMod(t, goModFixture))
	if c.Name() != "vuln-scan" {
		t.Errorf("Name() = %q, want vuln-scan", c.Name())
	}
}
