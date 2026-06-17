// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package checks

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeSecretsScanner is a deterministic SecretsScanner stub for unit tests.
type fakeSecretsScanner struct {
	findings []ports.SecretFinding
	err      error
	scanned  ports.ScanInput
}

func (f *fakeSecretsScanner) Scan(in ports.ScanInput) ([]ports.SecretFinding, error) {
	f.scanned = in
	if f.err != nil {
		return nil, f.err
	}
	return f.findings, nil
}

func TestSecretsScanCheck_AddedLinesHit_EmitsFindings(t *testing.T) {
	rawSecret := "AKIAIOSFODNN7EXAMPLE"
	scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{
		{Rule: "aws-access-key", FilePath: "config.go", Line: 12,
			Redacted: "AKIA****************", Confidence: 0.95},
		{Rule: "generic-api-key", FilePath: "config.go", Line: 30,
			Redacted: "sk-****", Confidence: 0.7},
	}}
	c := NewSecretsScanCheck(scanner)

	in := Input{
		RepoID: "repo1", Branch: "main",
		AddedLines: map[string][]Line{
			"config.go": {{Number: 12, Text: "key = " + rawSecret}},
		},
	}
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
		if f.Rule != "secret_leak" {
			t.Errorf("rule = %q, want secret_leak", f.Rule)
		}
		if f.FilePath == nil || *f.FilePath != "config.go" {
			t.Errorf("file anchor = %v, want config.go", f.FilePath)
		}
		if strings.Contains(f.Message, rawSecret) {
			t.Errorf("message leaks raw secret: %q", f.Message)
		}
	}
	if !strings.Contains(got[0].Message, "AKIA****************") {
		t.Errorf("message %q missing redacted snippet", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "aws-access-key") {
		t.Errorf("message %q missing rule name", got[0].Message)
	}
	// AddedLines must be forwarded verbatim to the scanner.
	if len(scanner.scanned.AddedLines["config.go"]) != 1 {
		t.Errorf("scanner did not receive added lines: %v", scanner.scanned)
	}
	if got[0].FindingID == got[1].FindingID {
		t.Error("findings on different lines must have distinct finding_ids")
	}
}

// TestSecretsScanCheck_RepoNamespacedFindingID guards: two repos
// that leak the SAME secret (same rule, file, and line) must NOT share a
// finding_id, or the second repo's promote upserts over the first on the
// (finding_id, branch) PK and the first repo silently loses the finding. The
// same secret in the SAME repo must stay idempotent (stable finding_id).
func TestSecretsScanCheck_RepoNamespacedFindingID(t *testing.T) {
	mkFindings := func() []ports.SecretFinding {
		return []ports.SecretFinding{
			{Rule: "aws-access-key", FilePath: "config.go", Line: 12,
				Redacted: "AKIA****************", Confidence: 0.95},
		}
	}
	run := func(repoID string) *domain.Finding {
		c := NewSecretsScanCheck(&fakeSecretsScanner{findings: mkFindings()})
		got, err := c.Run(context.Background(), Input{
			RepoID: repoID, Branch: "main",
			AddedLines: map[string][]Line{"config.go": {{Number: 12, Text: "key = AKIAIOSFODNN7EXAMPLE"}}},
		})
		if err != nil {
			t.Fatalf("Run(%s): %v", repoID, err)
		}
		if len(got) != 1 {
			t.Fatalf("Run(%s): want 1 finding, got %d", repoID, len(got))
		}
		return got[0]
	}

	repo1a := run("repo1")
	repo1b := run("repo1")
	repo2 := run("repo2")

	if repo1a.FindingID != repo1b.FindingID {
		t.Errorf("same repo+secret must be idempotent: %q != %q", repo1a.FindingID, repo1b.FindingID)
	}
	if repo1a.FindingID == repo2.FindingID {
		t.Errorf("different repos sharing a secret must get distinct finding_ids; both = %q", repo1a.FindingID)
	}
}

func TestSecretsScanCheck_EmptyAddedLines_NoFindings(t *testing.T) {
	scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{
		{Rule: "aws-access-key", FilePath: "config.go", Line: 1, Redacted: "x"},
	}}
	c := NewSecretsScanCheck(scanner)

	for _, in := range []Input{
		{RepoID: "repo1", Branch: "main"},
		{RepoID: "repo1", Branch: "main", AddedLines: map[string][]Line{}},
	} {
		got, err := c.Run(context.Background(), in)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want no findings, got %d", len(got))
		}
	}
}

// A pre-existing secret on an untouched line is excluded because AddedLines
// only carries added lines - the scanner never sees it, so no finding.
func TestSecretsScanCheck_PreexistingSecretOnUntouchedLine_NoFinding(t *testing.T) {
	scanner := &fakeSecretsScanner{} // sees no matching content -> no findings
	c := NewSecretsScanCheck(scanner)

	in := Input{
		RepoID: "repo1", Branch: "main",
		AddedLines: map[string][]Line{
			"config.go": {{Number: 5, Text: "// a harmless added comment"}},
		},
	}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no findings, got %d", len(got))
	}
}

func TestSecretsScanCheck_Idempotent(t *testing.T) {
	scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{
		{Rule: "aws-access-key", FilePath: "config.go", Line: 12, Redacted: "AKIA****"},
	}}
	c := NewSecretsScanCheck(scanner)
	in := Input{
		RepoID: "repo1", Branch: "main",
		AddedLines: map[string][]Line{
			"config.go": {{Number: 12, Text: "secret"}},
		},
	}

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

func TestSecretsScanCheck_ScannerError(t *testing.T) {
	scanner := &fakeSecretsScanner{err: strconvErr()}
	c := NewSecretsScanCheck(scanner)
	in := Input{
		AddedLines: map[string][]Line{"f.go": {{Number: 1, Text: "x"}}},
	}
	if _, err := c.Run(context.Background(), in); err == nil {
		t.Fatal("want error when scanner fails")
		return
	}
}

func strconvErr() error {
	_, err := strconv.Atoi("not-a-number")
	return err
}

func TestSecretsScanCheck_NilScanner(t *testing.T) {
	c := NewSecretsScanCheck(nil)
	in := Input{AddedLines: map[string][]Line{"f.go": {{Number: 1, Text: "x"}}}}
	if _, err := c.Run(context.Background(), in); err == nil {
		t.Fatal("want error on nil scanner")
		return
	}
}

func TestSecretsScanCheck_Name(t *testing.T) {
	c := NewSecretsScanCheck(&fakeSecretsScanner{})
	if c.Name() != "secrets-scan" {
		t.Errorf("Name() = %q, want secrets-scan", c.Name())
	}
}

// TestSecretsScanCheck_IgnoresBeadsTracker pins: high-entropy
// memory keys inside.beads/issues.jsonl must not surface as findings. The
// scanner must never see lines from that path, so the canonical scanner-stub
// records-input check is also asserted.
func TestSecretsScanCheck_IgnoresBeadsTracker(t *testing.T) {
	scanner := &fakeSecretsScanner{}
	c := NewSecretsScanCheck(scanner)
	in := Input{
		RepoID: "repo1", Branch: "main",
		AddedLines: map[string][]Line{
			".beads/issues.jsonl": {{Number: 586, Text: `{"_type":"memory","key":"high-entropy-slug-here"}`}},
		},
	}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 findings on ignored beads tracker, got %d", len(got))
	}
	if _, present := scanner.scanned.AddedLines[".beads/issues.jsonl"]; present {
		t.Error("scanner received .beads/issues.jsonl input; ignore filter did not run before scan")
	}
}

// TestSecretsScanCheck_IgnoresVendoredDeps pins: a freshly
// vendored cobra (or any Go vendor/ tree) must not trip the high-entropy
// rule on Apache 2 license URLs and bash-completion boilerplate. The same
// filter also covers node_modules/, third_party/, and nested-segment matches
// like apps/foo/vendor/.
func TestSecretsScanCheck_IgnoresVendoredDeps(t *testing.T) {
	for _, path := range []string{
		"vendor/github.com/spf13/cobra/cobra.go",
		"node_modules/lodash/package.json",
		"third_party/protobuf/descriptor.proto",
		"apps/cli/vendor/github.com/spf13/pflag/float64_slice.go",
		"services/api/node_modules/express/index.js",
	} {
		scanner := &fakeSecretsScanner{}
		c := NewSecretsScanCheck(scanner)
		in := Input{
			RepoID: "repo1", Branch: "main",
			AddedLines: map[string][]Line{
				path: {{Number: 7, Text: "//      http://www.apache.org/licenses/LICENSE-2.0"}},
			},
		}
		got, err := c.Run(context.Background(), in)
		if err != nil {
			t.Fatalf("Run(%s): %v", path, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: want 0 findings, got %d", path, len(got))
		}
		if _, present := scanner.scanned.AddedLines[path]; present {
			t.Errorf("%s: scanner received input; ignore filter did not run before scan", path)
		}
	}
}

// TestSecretsScanCheck_IgnoresTestFixturePEM pins: PEM /
// key-shaped credentials living under conventional test-fixture paths
// (testdata/, /test/ subtrees, test/fixtures/) are vanishingly unlikely to
// be real secrets and routinely flood ephemeral / forked repos like
// dgrijalva/jwt-go. The path-based filter applies regardless of repo kind
// (a tracked repo's test fixtures are equally noisy).
func TestSecretsScanCheck_IgnoresTestFixturePEM(t *testing.T) {
	for _, path := range []string{
		"testdata/privateSecure.pem",
		"internal/foo/testdata/key.pem",
		"test/fixtures/sample.key",
		"test/keys/rsa.pem",
		"pkg/test/fixtures/private.pem",
		"crypto/testdata/cert.crt",
	} {
		scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{
			{Rule: "private-key", FilePath: path, Line: 1, Redacted: "-----BEGIN ...", Confidence: 0.95},
		}}
		c := NewSecretsScanCheck(scanner)
		in := Input{
			RepoID: "r", Branch: "main",
			AddedLines: map[string][]Line{
				path: {{Number: 1, Text: "-----BEGIN RSA PRIVATE KEY-----"}},
			},
		}
		got, err := c.Run(context.Background(), in)
		if err != nil {
			t.Fatalf("Run(%s): %v", path, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: want 0 findings (test-fixture key), got %d", path, len(got))
		}
		if _, present := scanner.scanned.AddedLines[path]; present {
			t.Errorf("%s: scanner received input; fixture filter did not run before scan", path)
		}
	}
}

// TestSecretsScanCheck_NonFixturePEMStillFlags verifies the fixture filter
// is targeted: a.pem in a production / non-test path still reports.
func TestSecretsScanCheck_NonFixturePEMStillFlags(t *testing.T) {
	for _, path := range []string{
		"config/private.pem",   // production-looking dir
		"deploy/tls.key",       // ops dir
		"testify_helper.go",    // substring of "test" but not a fixture path segment
		"contestdata/file.pem", // substring of "testdata" but not a segment
	} {
		scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{
			{Rule: "private-key", FilePath: path, Line: 1, Redacted: "-----BEGIN ...", Confidence: 0.95},
		}}
		c := NewSecretsScanCheck(scanner)
		in := Input{
			RepoID: "r", Branch: "main",
			AddedLines: map[string][]Line{
				path: {{Number: 1, Text: "-----BEGIN RSA PRIVATE KEY-----"}},
			},
		}
		got, err := c.Run(context.Background(), in)
		if err != nil {
			t.Fatalf("Run(%s): %v", path, err)
		}
		if len(got) != 1 {
			t.Errorf("%s: want 1 finding (non-fixture path), got %d", path, len(got))
		}
	}
}

// TestSecretsScanCheck_CollapsesMultiLinePEM verifies that many consecutive
// line-hits from the same rule on the same file collapse to a single
// finding with a count suffix in the message. Without this, a 28-line PEM
// block flagged line-by-line produces 28 separate findings - the dominant
// failure mode of `veska search --repo` on jwt-go.
func TestSecretsScanCheck_CollapsesMultiLinePEM(t *testing.T) {
	const path = "config/private.pem" // non-fixture path so we reach the scanner
	var raw []ports.SecretFinding
	for i := 1; i <= 28; i++ {
		raw = append(raw, ports.SecretFinding{
			Rule: "private-key", FilePath: path, Line: i,
			Redacted: "***", Confidence: 0.95,
		})
	}
	scanner := &fakeSecretsScanner{findings: raw}
	c := NewSecretsScanCheck(scanner)
	lines := make([]Line, 0, 28)
	for i := 1; i <= 28; i++ {
		lines = append(lines, Line{Number: i, Text: "key"})
	}
	in := Input{
		RepoID: "r", Branch: "main",
		AddedLines: map[string][]Line{path: lines},
	}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed finding for 28-line PEM, got %d", len(got))
	}
	// The message should indicate additional matches (collapsed) - either
	// the raw count of additional lines or a total. We assert the "+27
	// more" form to pin the chosen surface.
	if !strings.Contains(got[0].Message, "27") {
		t.Errorf("collapsed message should mention the additional-match count, got %q", got[0].Message)
	}
}

// TestSecretsScanCheck_VendorSubstringDoesNotMatch guards against the
// pathHasSegment check falsely matching e.g. "vendored_data/" or
// "my_vendor.go" (substring of `vendor`).
func TestSecretsScanCheck_VendorSubstringDoesNotMatch(t *testing.T) {
	scanner := &fakeSecretsScanner{findings: []ports.SecretFinding{{
		FilePath: "vendored_data/keys.txt", Line: 1, Rule: "high-entropy", Redacted: "***", Confidence: 0.5,
	}}}
	c := NewSecretsScanCheck(scanner)
	in := Input{
		RepoID: "repo1", Branch: "main",
		AddedLines: map[string][]Line{
			"vendored_data/keys.txt": {{Number: 1, Text: "AKIA....."}},
		},
	}
	got, err := c.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("want 1 finding on vendored_data/ (substring of vendor), got %d", len(got))
	}
}
