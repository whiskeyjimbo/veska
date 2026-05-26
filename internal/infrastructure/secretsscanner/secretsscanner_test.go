package secretsscanner_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
)

// Compile-time interface satisfaction check.
var _ ports.SecretsScanner = (*secretsscanner.BuiltinScanner)(nil)

func TestBuiltinScanner_Scan(t *testing.T) {
	t.Parallel()

	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const ghToken = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	const entropyTok = "h8Kq2Lx9Zp4Wn7Vc3Mb6Td1Rj5Yf0Gs"

	tests := []struct {
		name       string
		text       string
		wantFinds  bool
		wantRule   string // expected rule name when wantFinds; "" to skip check
		rawSecrets []string
	}{
		{
			name:       "aws access key id",
			text:       `key := "` + awsKey + `"`,
			wantFinds:  true,
			wantRule:   "aws-access-key-id",
			rawSecrets: []string{awsKey},
		},
		{
			name:      "private key pem header",
			text:      "-----BEGIN RSA PRIVATE KEY-----",
			wantFinds: true,
			wantRule:  "private-key",
		},
		{
			name:       "github token",
			text:       "token=" + ghToken,
			wantFinds:  true,
			wantRule:   "github-token",
			rawSecrets: []string{ghToken},
		},
		{
			name:       "high entropy string no rule",
			text:       `payload(` + entropyTok + `)`,
			wantFinds:  true,
			wantRule:   "high-entropy",
			rawSecrets: []string{entropyTok},
		},
		{
			name:      "ordinary code line",
			text:      "func computeBlastRadius(graph *Graph) int {",
			wantFinds: false,
		},
		{
			name:      "ordinary prose comment",
			text:      "// This function returns the number of affected nodes.",
			wantFinds: false,
		},
		{
			name:      "short identifier",
			text:      "x := configValue + defaultTimeout",
			wantFinds: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := secretsscanner.New()
			in := ports.ScanInput{AddedLines: map[string][]ports.Line{
				"file.go": {{Number: 7, Text: tt.text}},
			}}
			got, err := s.Scan(in)
			if err != nil {
				t.Fatalf("Scan returned error: %v", err)
			}
			if tt.wantFinds && len(got) == 0 {
				t.Fatalf("expected at least one finding, got none")
			}
			if !tt.wantFinds && len(got) != 0 {
				t.Fatalf("expected no findings, got %d: %+v", len(got), got)
			}
			for _, f := range got {
				if f.FilePath != "file.go" {
					t.Errorf("FilePath = %q, want file.go", f.FilePath)
				}
				if f.Line != 7 {
					t.Errorf("Line = %d, want 7", f.Line)
				}
				if f.Rule == "" {
					t.Errorf("finding has empty Rule")
				}
				if f.Confidence < 0 || f.Confidence > 1 {
					t.Errorf("Confidence = %v, out of 0..1", f.Confidence)
				}
				for _, raw := range tt.rawSecrets {
					if strings.Contains(f.Redacted, raw) {
						t.Errorf("Redacted %q contains raw secret %q", f.Redacted, raw)
					}
				}
			}
			if tt.wantFinds && tt.wantRule != "" {
				var found bool
				for _, f := range got {
					if f.Rule == tt.wantRule {
						found = true
					}
				}
				if !found {
					t.Errorf("no finding with rule %q; got %+v", tt.wantRule, got)
				}
			}
		})
	}
}

// TestBuiltinScanner_ImportPathsNotFlagged pins solov2-j66g + solov2-1rfo:
// Go import paths and well-known module URLs must never produce a finding —
// they cross the entropy threshold but are never secrets.
func TestBuiltinScanner_ImportPathsNotFlagged(t *testing.T) {
	t.Parallel()
	s := secretsscanner.New()
	cases := []string{
		`_ "github.com/dgrijalva/jwt-go"`,
		`import "go.opentelemetry.io/otel/trace"`,
		`"google.golang.org/grpc/credentials"`,
		`"k8s.io/apimachinery/pkg/runtime"`,
		`"gopkg.in/yaml.v3"`,
	}
	for _, txt := range cases {
		t.Run(txt, func(t *testing.T) {
			in := ports.ScanInput{AddedLines: map[string][]ports.Line{
				"file.go": {{Number: 1, Text: txt}},
			}}
			got, err := s.Scan(in)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("expected no findings for import-path-like line %q, got %+v", txt, got)
			}
		})
	}
}

// TestBuiltinScanner_GitleaksAddsStripeRule pins solov2-j66g: gitleaks
// fires on Stripe test tokens that the previous local-only rules missed.
// Uses a synthetic-but-realistic shape that gitleaks does not allowlist.
func TestBuiltinScanner_GitleaksAddsStripeRule(t *testing.T) {
	t.Parallel()
	s := secretsscanner.New()
	in := ports.ScanInput{AddedLines: map[string][]ports.Line{
		"file.go": {{Number: 1, Text: `pw := "sk_test_4eC39HqLyjWDarjtT1zdp7dc"`}},
	}}
	got, err := s.Scan(in)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var sawStripe bool
	for _, f := range got {
		if strings.Contains(f.Rule, "stripe") {
			sawStripe = true
		}
	}
	if !sawStripe {
		t.Errorf("expected a stripe-* rule finding (gitleaks); got %+v", got)
	}
}

func TestBuiltinScanner_EmptyInput(t *testing.T) {
	t.Parallel()
	s := secretsscanner.New()

	got, err := s.Scan(ports.ScanInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings, got %v", got)
	}

	got, err = s.Scan(ports.ScanInput{AddedLines: map[string][]ports.Line{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil findings for empty map, got %v", got)
	}
}

func TestBuiltinScanner_ExcludesLockfiles(t *testing.T) {
	t.Parallel()
	s := secretsscanner.New()
	// A real go.sum line: high-entropy hash, but never a secret.
	const goSumLine = "github.com/spf13/cobra v1.10.2 h1:DM3sOC6FYNAhJ8K0jK3K7vKQ8gQXl8Rh+nQ7eAQpQyU="
	for _, path := range []string{"go.sum", "go.mod", "vendor/foo/go.sum", "package-lock.json", "Cargo.lock"} {
		in := ports.ScanInput{AddedLines: map[string][]ports.Line{
			path: {{Number: 1, Text: goSumLine}},
		}}
		got, err := s.Scan(in)
		if err != nil {
			t.Fatalf("Scan(%s) error: %v", path, err)
		}
		if len(got) != 0 {
			t.Errorf("Scan(%s) produced %d findings, want 0", path, len(got))
		}
	}
}

func TestBuiltinScanner_RedactionMasksValue(t *testing.T) {
	t.Parallel()
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	s := secretsscanner.New()
	in := ports.ScanInput{AddedLines: map[string][]ports.Line{
		"creds.go": {{Number: 1, Text: awsKey}},
	}}
	got, err := s.Scan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected a finding")
	}
	if !strings.Contains(got[0].Redacted, "*") {
		t.Errorf("Redacted %q does not appear masked", got[0].Redacted)
	}
}
