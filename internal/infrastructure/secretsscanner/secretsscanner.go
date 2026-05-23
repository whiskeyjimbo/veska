// Package secretsscanner provides the veska-builtin SecretsScanner: an
// in-process detector combining named regex rules for well-known secret
// shapes with a Shannon-entropy heuristic for unrecognised high-entropy
// tokens. Redaction is built in, so a raw secret value never reaches a
// finding. It is fast enough to run on every promotion.
package secretsscanner

import (
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// excludedBaseNames are file names whose contents are inherently high-entropy
// but never carry user secrets — language/package lockfiles and manifests.
// Scanning these produces only false positives.
var excludedBaseNames = map[string]struct{}{
	"go.sum":            {},
	"go.mod":            {},
	"package-lock.json": {},
	"yarn.lock":         {},
	"pnpm-lock.yaml":    {},
	"Cargo.lock":        {},
	"poetry.lock":       {},
	"Pipfile.lock":      {},
	"composer.lock":     {},
	"Gemfile.lock":      {},
	"flake.lock":        {},
}

// isExcludedPath reports whether path's basename matches a known lockfile or
// manifest whose entropy is structural, not secret-bearing.
func isExcludedPath(path string) bool {
	_, ok := excludedBaseNames[filepath.Base(path)]
	return ok
}

// rule pairs a detection name with the pattern that recognises a secret shape.
// The submatch group, when present, isolates the sensitive value to redact;
// otherwise the whole match is redacted.
type rule struct {
	name       string
	re         *regexp.Regexp
	confidence float64
}

// builtinRules are compiled once at package init and shared read-only across
// all scanners and goroutines — regexp.Regexp is safe for concurrent use.
var builtinRules = []rule{
	{
		name:       "aws-access-key-id",
		re:         regexp.MustCompile(`\b((?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[A-Z0-9]{16})\b`),
		confidence: 0.95,
	},
	{
		name:       "private-key",
		re:         regexp.MustCompile(`(-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----)`),
		confidence: 0.99,
	},
	{
		name:       "github-token",
		re:         regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36})\b`),
		confidence: 0.95,
	},
	{
		name:       "slack-token",
		re:         regexp.MustCompile(`\b(xox[baprs]-[A-Za-z0-9-]{10,})\b`),
		confidence: 0.9,
	},
	{
		name:       "generic-api-key",
		re:         regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|password)\s*[:=]\s*["']([A-Za-z0-9_\-./+]{16,})["']`),
		confidence: 0.6,
	},
}

const (
	// entropyThreshold is the Shannon entropy (bits/char) above which a token
	// is treated as secret-shaped. Ordinary identifiers and prose sit well
	// below this; random credential strings sit above it.
	entropyThreshold = 4.0

	// entropyMinLen is the shortest token the entropy heuristic considers;
	// short tokens cannot reach a meaningful entropy and produce false hits.
	entropyMinLen = 24

	// entropyConfidence is the per-finding confidence for entropy-only hits.
	entropyConfidence = 0.5
)

// tokenRe splits a line into candidate secret tokens: runs of characters that
// commonly appear in credentials. Prose words break on the missing digits.
var tokenRe = regexp.MustCompile(`[A-Za-z0-9_\-./+]{` +
	itoa(entropyMinLen) + `,}`)

// itoa converts a small non-negative int to its decimal string, avoiding a
// strconv import for a single compile-time constant.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// BuiltinScanner is the in-process SecretsScanner. It holds no mutable state
// and is safe for concurrent use.
type BuiltinScanner struct {
	entropyThreshold float64
}

// Compile-time interface satisfaction check.
var _ ports.SecretsScanner = (*BuiltinScanner)(nil)

// Option configures a BuiltinScanner at construction time.
type Option func(*BuiltinScanner)

// WithEntropyThreshold overrides the Shannon-entropy threshold (bits/char)
// above which an unrecognised token is flagged. Values <= 0 are ignored.
func WithEntropyThreshold(bitsPerChar float64) Option {
	return func(s *BuiltinScanner) {
		if bitsPerChar > 0 {
			s.entropyThreshold = bitsPerChar
		}
	}
}

// New constructs a BuiltinScanner with the default rule set and entropy
// threshold, applying any options. It has no required dependencies.
func New(opts ...Option) *BuiltinScanner {
	s := &BuiltinScanner{entropyThreshold: entropyThreshold}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Scan inspects every added line, applying the named regex rules first and the
// entropy heuristic for tokens no rule claimed. A nil/empty input yields
// (nil, nil).
func (s *BuiltinScanner) Scan(in ports.ScanInput) ([]ports.SecretFinding, error) {
	if len(in.AddedLines) == 0 {
		return nil, nil
	}
	var findings []ports.SecretFinding
	for path, lines := range in.AddedLines {
		if isExcludedPath(path) {
			continue
		}
		for _, line := range lines {
			findings = append(findings, s.scanLine(path, line)...)
		}
	}
	if len(findings) == 0 {
		return nil, nil
	}
	return findings, nil
}

// scanLine returns the findings for a single line.
func (s *BuiltinScanner) scanLine(path string, line ports.Line) []ports.SecretFinding {
	var findings []ports.SecretFinding
	matched := map[string]struct{}{} // raw values already claimed by a rule

	for _, r := range builtinRules {
		m := r.re.FindStringSubmatch(line.Text)
		if m == nil {
			continue
		}
		raw := m[0]
		if len(m) > 1 && m[1] != "" {
			raw = m[1]
		}
		matched[raw] = struct{}{}
		findings = append(findings, ports.SecretFinding{
			Rule:       r.name,
			FilePath:   path,
			Line:       line.Number,
			Redacted:   redactLine(line.Text, raw),
			Confidence: r.confidence,
		})
	}

	for _, tok := range tokenRe.FindAllString(line.Text, -1) {
		if _, claimed := matched[tok]; claimed {
			continue
		}
		if shannonEntropy(tok) < s.entropyThreshold {
			continue
		}
		matched[tok] = struct{}{}
		findings = append(findings, ports.SecretFinding{
			Rule:       "high-entropy",
			FilePath:   path,
			Line:       line.Number,
			Redacted:   redactLine(line.Text, tok),
			Confidence: entropyConfidence,
		})
	}
	return findings
}

// redactLine replaces the secret substring within the line with a mask of the
// same length, keeping surrounding context intact so the finding stays
// readable without exposing the raw value.
func redactLine(text, secret string) string {
	return strings.ReplaceAll(text, secret, mask(secret))
}

// mask returns a masked stand-in for a secret: the first two characters are
// kept as a hint, the rest become asterisks. Short secrets are fully masked.
func mask(secret string) string {
	if len(secret) <= 4 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:2] + strings.Repeat("*", len(secret)-2)
}

// shannonEntropy returns the Shannon entropy of s in bits per character.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
