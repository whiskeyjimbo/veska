// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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

// TestVulnScanCheck_MessageCarriesGoModLine pins: the
// finding message must include the go.mod line of the offending require
// so editors can jump to source. (The findings table has no dedicated
// line column today - see also the schema note in the issue.)
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

// TestVulnScanCheck_GoModNotTouched_StillScans pins: a
// promotion whose changed file set does not include go.mod must still
// run vuln-scan against the on-disk go.mod, so retroactive scans after
// enabling [vuln_source] (via partial re-promote in `config reload`)
// surface findings.
func TestVulnScanCheck_GoModNotTouched_StillScans(t *testing.T) {
	src := &fakeVulnSource{findings: []ports.VulnFinding{
		{AdvisoryID: "GHSA-aaaa-bbbb-cccc", Package: "p", Severity: "HIGH"},
	}}
	c := NewVulnScanCheck(src, writeGoMod(t, goModFixture))

	in := Input{RepoID: "repo1", Branch: "main", FilePaths: []string{"main.go", "README.md"}}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding (scan must run despite go.mod not in FilePaths), got %d", len(got))
	}
}

// TestVulnScanCheck_NoGoModOnDisk_NoFindings pins the non-Go-repo case
// after the touchesGoMod gate was dropped: a repo with no go.mod at all
// is a no-op, not an error.
func TestVulnScanCheck_NoGoModOnDisk_NoFindings(t *testing.T) {
	src := &fakeVulnSource{}
	// repoRoot points at an empty dir - no go.mod present.
	emptyRoot := t.TempDir()
	repoRoot := func(_ context.Context, _ string) (string, error) { return emptyRoot, nil }
	c := NewVulnScanCheck(src, repoRoot)

	got, err := c.Run(context.Background(), Input{RepoID: "r", Branch: "main"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 findings on no-go.mod repo, got %d", len(got))
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

// TestVulnScanCheck_RepoNamespacedFindingID pins: two repos
// that share the SAME advisory (same AdvisoryID+Package) must each retain
// their OWN vulnerable_dependency finding. Because the storage PK is
// (finding_id, branch) with no repo_id, identical finding_ids on the same
// branch would let one repo's scan overwrite the other's row - silently
// dropping a real CVE from all-but-one repo. The finding_id must therefore
// be namespaced by repo id while staying idempotent within a repo.
func TestVulnScanCheck_RepoNamespacedFindingID(t *testing.T) {
	advisory := []ports.VulnFinding{
		{AdvisoryID: "GHSA-w73w-5m7g-f7qc", Package: "github.com/dgrijalva/jwt-go",
			AffectedRange: "<4.0.0", Severity: "HIGH", Summary: "jwt bypass"},
	}
	newCheck := func() *VulnScanCheck {
		return NewVulnScanCheck(&fakeVulnSource{findings: advisory}, writeGoMod(t, goModFixture))
	}

	repo1a, err := newCheck().Run(context.Background(), Input{RepoID: "repoA", Branch: "main"})
	if err != nil {
		t.Fatalf("Run repoA: %v", err)
	}
	repo2, err := newCheck().Run(context.Background(), Input{RepoID: "repoB", Branch: "main"})
	if err != nil {
		t.Fatalf("Run repoB: %v", err)
	}
	if len(repo1a) != 1 || len(repo2) != 1 {
		t.Fatalf("want 1 finding per repo, got %d / %d", len(repo1a), len(repo2))
	}
	if repo1a[0].FindingID == repo2[0].FindingID {
		t.Errorf("finding_id collides across repos: repoA=%q repoB=%q", repo1a[0].FindingID, repo2[0].FindingID)
	}

	// Idempotency: re-scanning the SAME repo+advisory yields the SAME id.
	repo1b, err := newCheck().Run(context.Background(), Input{RepoID: "repoA", Branch: "main"})
	if err != nil {
		t.Fatalf("Run repoA again: %v", err)
	}
	if len(repo1b) != 1 {
		t.Fatalf("want 1 finding on re-scan, got %d", len(repo1b))
	}
	if repo1a[0].FindingID != repo1b[0].FindingID {
		t.Errorf("finding_id not stable within repo: %q != %q", repo1a[0].FindingID, repo1b[0].FindingID)
	}
}

func TestVulnScanCheck_NilSource(t *testing.T) {
	c := NewVulnScanCheck(nil, writeGoMod(t, goModFixture))
	if _, err := c.Run(context.Background(), Input{FilePaths: []string{"go.mod"}}); err == nil {
		t.Fatal("want error on nil source")
		return
	}
}

// TestVulnScanCheck_GoModReadFailure pins: a missing go.mod
// is no longer an error after the touchesGoMod gate removal - non-Go repos
// are a normal case (TestVulnScanCheck_NoGoModOnDisk_NoFindings covers that).
// This test now pins the residual error path: a read failure that ISN'T
// os.IsNotExist (e.g. a directory at the go.mod path) still surfaces.
func TestVulnScanCheck_GoModReadFailure(t *testing.T) {
	dir := t.TempDir()
	// Create a *directory* at go.mod so os.ReadFile errors with EISDIR,
	// which is not IsNotExist.
	if err := os.Mkdir(filepath.Join(dir, "go.mod"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	root := func(ctx context.Context, repoID string) (string, error) { return dir, nil }
	c := NewVulnScanCheck(&fakeVulnSource{}, root)
	if _, err := c.Run(context.Background(), Input{RepoID: "r", Branch: "main"}); err == nil {
		t.Fatal("want error when go.mod is unreadable (directory at that path)")
	}
}

// TestVulnScanCheck_GoModTouched_AbsolutePath is the regression for
// the cold-scan / fsnotify Save path passes full filesystem
// paths through to PromotionBatch.Files.Path, so the gate must match
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
