// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/manifest"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"os"
)

// TestRunSecurity_E2E_SecretFailsNonZero drives the WIRED command path
// RunSecurity -> git.AddedLinesBetween -> the REAL secretsscanner -> JSON report
// + ErrGateFailed (non-zero exit). The candidate adds a real AWS-key-shaped
// secret; vuln is unconfigured (VESKA_HOME is empty), so this exercises the
// secrets dimension and the DoD exit behavior end to end.
func TestRunSecurity_E2E_SecretFailsNonZero(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir()) // isolate config; no vuln source configured

	repoDir := t.TempDir()
	withSecret := "package p\n\nconst k = \"AKIAZQ7XFAKE1234ABCD\"\n"
	makeRepo(t, repoDir,
		map[string]string{"main.go": "package p\n"},  // base: clean
		map[string]*string{"secret.go": &withSecret}, // candidate: adds a secret
	)

	var out bytes.Buffer
	err := RunSecurity(context.Background(), SecurityParams{
		RepoID: "r", Branch: "main", RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("expected ErrGateFailed (non-zero exit); got %v\nraw: %s", err, out.String())
	}
	var rep struct {
		Pass           bool     `json:"pass"`
		Failures       []string `json:"failures"`
		VulnApplicable bool     `json:"vuln_applicable"`
		NewSecretLeaks []struct {
			Rule string `json:"rule"`
		} `json:"new_secret_leaks"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	if rep.Pass {
		t.Fatalf("expected FAIL; got %s", out.String())
	}
	if len(rep.NewSecretLeaks) != 1 || rep.NewSecretLeaks[0].Rule != "secret_leak" {
		t.Fatalf("expected one secret_leak; got %+v", rep.NewSecretLeaks)
	}
	if len(rep.Failures) != 1 || rep.Failures[0] != diffgate.FailNewSecretLeak {
		t.Fatalf("failures = %v", rep.Failures)
	}
	if rep.VulnApplicable {
		t.Fatalf("vuln should be not-applicable with no source configured")
	}
}

// TestRunSecurity_E2E_EmptyCacheDegrades is the security-sensitive fail-safe:
// vuln_source=osv is configured but the advisory cache was never refreshed
// (osv.Adapter.Scan returns nil,nil on an empty cache - no error). A candidate
// touching go.mod must NOT silently PASS; the gate degrades to a FAIL with
// vuln_unchecked. Without the cache-readiness guard this would be a false green.
func TestRunSecurity_E2E_EmptyCacheDegrades(t *testing.T) {
	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)
	// Enable the osv provider; leave $VESKA_HOME/cache/osv absent (unrefreshed).
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[vuln_source]\nprovider = \"osv\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	repoDir := t.TempDir()
	baseMod := "module example.com/m\n\ngo 1.21\n\nrequire example.com/dep v1.0.0\n"
	candMod := "module example.com/m\n\ngo 1.21\n\nrequire example.com/dep v1.1.0\n"
	makeRepo(t, repoDir,
		map[string]string{"go.mod": baseMod},
		map[string]*string{"go.mod": &candMod},
	)

	var out bytes.Buffer
	err := RunSecurity(context.Background(), SecurityParams{
		RepoID: "r", Branch: "main", RepoRoot: repoDir,
		BaseRef: "HEAD~1", CandidateRef: "HEAD", Out: &out,
	})
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("empty cache + configured vuln must FAIL (not silent pass); got %v\nraw: %s", err, out.String())
	}
	var rep struct {
		Pass           bool     `json:"pass"`
		Failures       []string `json:"failures"`
		VulnApplicable bool     `json:"vuln_applicable"`
		VulnChecked    bool     `json:"vuln_checked"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("verdict JSON: %v\nraw: %s", jerr, out.String())
	}
	if rep.Pass || !rep.VulnApplicable || rep.VulnChecked {
		t.Fatalf("want applicable+unchecked degraded FAIL; got %+v", rep)
	}
	if len(rep.Failures) != 1 || rep.Failures[0] != diffgate.FailVulnUnchecked {
		t.Fatalf("failures = %v, want [vuln_unchecked]", rep.Failures)
	}
}

const testAdvisory = `{
  "id": "GO-TEST-0001",
  "summary": "Test vuln in example.com/vuln",
  "severity": [{"type": "CVSS_V3", "score": "7.5"}],
  "affected": [{
    "package": {"ecosystem": "Go", "name": "example.com/vuln"},
    "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "2.0.0"}]}]
  }]
}`

// TestSecurityGate_RealOSV_NewVulnFails drives the REAL OSV adapter (fixture
// cache) and the REAL manifest.ReadGoMod through the gate: the candidate go.mod
// adds a require on a module the advisory cache flags as vulnerable, absent at
// base. Proves AC2 net-new vuln detection over the real scanner + real parser,
// diffed by finding_id.
func TestSecurityGate_RealOSV_NewVulnFails(t *testing.T) {
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "GO-TEST-0001.json"), []byte(testAdvisory), 0o644); err != nil {
		t.Fatalf("write advisory: %v", err)
	}
	src := osv.New(osv.WithCacheDir(cacheDir))

	scanDeps := func(ctx context.Context, repoID, branch, manifestPath string, deps []ports.Dependency) ([]*domain.Finding, error) {
		return checks.ScanManifestDeps(ctx, src, repoID, branch, manifestPath, deps, nil)
	}
	readers := map[string]diffgate.ManifestReaderFn{"go.mod": manifest.ReadGoMod}
	gate := diffgate.NewSecurityGate(
		func(context.Context, checks.Input) ([]*domain.Finding, error) { return nil, nil }, // secrets: none
		scanDeps, readers, true,
	)

	baseMod := "module example.com/m\n\ngo 1.21\n"
	candMod := "module example.com/m\n\ngo 1.21\n\nrequire example.com/vuln v1.0.0\n"
	readAtRef := func(_ context.Context, path, ref string) ([]byte, bool, error) {
		if path != "go.mod" {
			return nil, false, nil
		}
		if ref == "cand" {
			return []byte(candMod), true, nil
		}
		return []byte(baseMod), true, nil
	}

	v, err := gate.Evaluate(context.Background(), diffgate.SecurityInput{
		RepoID: "r", Branch: "main", BaseRef: "base", CandRef: "cand", ReadAtRef: readAtRef,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || len(v.NewVulnDeps) != 1 {
		t.Fatalf("real-OSV net-new vuln must FAIL with 1 finding; got %+v", v)
	}
	if v.NewVulnDeps[0].Rule != "vulnerable_dependency" {
		t.Fatalf("rule = %q", v.NewVulnDeps[0].Rule)
	}
}
