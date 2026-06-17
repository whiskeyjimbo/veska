package secretsscanner

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TestTrailingSentencePunctuationFalsePositive ensures that dotted/dashed identifiers
// at the end of sentences (such as in Godoc comments) are correctly recognized as
// identifiers and do not trigger false positive findings.
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

