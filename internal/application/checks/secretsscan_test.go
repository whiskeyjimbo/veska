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
// only carries added lines — the scanner never sees it, so no finding.
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

// TestSecretsScanCheck_IgnoresBeadsTracker pins solov2-jtl5.3: high-entropy
// memory keys inside .beads/issues.jsonl must not surface as findings. The
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

// TestSecretsScanCheck_IgnoresVendoredDeps pins solov2-l7zd: a freshly
// vendored cobra (or any Go vendor/ tree) must not trip the high-entropy
// rule on Apache 2 license URLs and bash-completion boilerplate. The same
// filter also covers node_modules/, third_party/, and nested-segment matches
// like apps/foo/vendor/...
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
