package secretsscanner

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TestTrailingSentencePunctuationFalsePositive reproduces the pflag
// text.go:62 false positive: an in-prose dotted identifier ending at a
// sentence period ("encoding.TextUnmarshaler.") was treated as non-
// identifier-shaped because identifierChainSeparators emitted an empty
// trailing element, causing the high-entropy rule to fire on a Godoc
// comment. Discovered during the junior onboarding journey against
// spf13/pflag.
func TestTrailingSentencePunctuationFalsePositive(t *testing.T) {
	s := New()
	cases := []string{
		"// The argument p must implement encoding.TextUnmarshaler. If the flag is used.",
		"// see io.ReadWriteCloser- for the contract.",
		"// path/to/some/component/ trailing slash.",
	}
	for _, text := range cases {
		in := ports.ScanInput{AddedLines: map[string][]ports.Line{
			"text.go": {{Number: 1, Text: text}},
		}}
		got, err := s.Scan(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range got {
			t.Errorf("unexpected finding on %q: rule=%s redacted=%q", text, f.Rule, f.Redacted)
		}
	}
}
