// Package secretsscanner provides an in-process secrets detector. It runs gitleaks
// for rule-based detection and falls back to a local regex and Shannon-entropy heuristic
// if gitleaks fails to initialize. Redaction is built-in to prevent raw secrets from
// being exposed in findings. The local fallback path serves as a safety net to ensure
// scanning is not silently disabled if gitleaks initialization fails.
package secretsscanner

import (
	"log/slog"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	gitleaksconfig "github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// docsExampleSecrets contains well-known placeholder credential strings that vendors publish
// in their documentation. Flagging these placeholders creates unnecessary noise during initial setups.
// Since real credentials should not match these literal strings, they are explicitly allowed.
var docsExampleSecrets = map[string]struct{}{
	"AKIAIOSFODNN7EXAMPLE":                     {},
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": {},
}

// isDocsExampleSecret reports whether raw is one of the vendor documentation placeholders.
func isDocsExampleSecret(raw string) bool {
	_, ok := docsExampleSecrets[raw]
	return ok
}

// excludedBaseNames lists lockfiles and manifests that are naturally high-entropy but
// do not contain user secrets, preventing false positives during scans.
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

// isExcludedPath reports whether the path points to a known lockfile or manifest.
func isExcludedPath(path string) bool {
	_, ok := excludedBaseNames[filepath.Base(path)]
	return ok
}

// rule maps a detection name to a regex pattern. The first submatch group isolates the
// secret for redaction if present; otherwise the entire match is redacted.
type rule struct {
	name       string
	re         *regexp.Regexp
	confidence float64
}

// builtinRules are compiled on initialization and shared concurrently.
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
	// entropyThreshold specifies the Shannon entropy bits/char limit above which a token is flagged.
	entropyThreshold = 4.0

	// entropyMinLen defines the minimum character length to evaluate for entropy.
	entropyMinLen = 24

	// entropyConfidence is the confidence rating assigned to entropy-only findings.
	entropyConfidence = 0.5
)

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_\-./+]{` +
	strconv.Itoa(entropyMinLen) + `,}`)

// BuiltinScanner detects secrets in added lines. It holds no mutable state
// after construction and is safe for concurrent use.
type BuiltinScanner struct {
	entropyThreshold float64
	// gitleaks acts as the primary detector. If initialization fails,
	// it is nil, and the scanner falls back to the local regex and entropy paths.
	gitleaks *detect.Detector
}

var _ ports.SecretsScanner = (*BuiltinScanner)(nil)

type Option func(*BuiltinScanner)

func WithEntropyThreshold(bitsPerChar float64) Option {
	return func(s *BuiltinScanner) {
		if bitsPerChar > 0 {
			s.entropyThreshold = bitsPerChar
		}
	}
}

// New constructs a BuiltinScanner. It initializes gitleaks with its default config.
// If initialization fails, the scanner logs a warning and falls back to local regex and
// entropy heuristics. It does not return errors to avoid blocking processes when a rule
// configuration is malformed.
func New(opts ...Option) *BuiltinScanner {
	s := &BuiltinScanner{entropyThreshold: entropyThreshold}
	for _, opt := range opts {
		opt(s)
	}
	if d, err := newGitleaksDetector(); err == nil {
		s.gitleaks = d
	} else {
		slog.Warn("secretsscanner: gitleaks init failed; falling back to local rules", "error", err)
	}
	return s
}

// newGitleaksDetector loads the embedded default gitleaks configuration through viper.
// This default configuration includes the necessary allowlists (such as Go imports and
// public package URLs) to minimize false positives.
func newGitleaksDetector() (*detect.Detector, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(gitleaksconfig.DefaultConfig)); err != nil {
		return nil, err
	}
	var vc gitleaksconfig.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return nil, err
	}
	cfg, err := vc.Translate()
	if err != nil {
		return nil, err
	}
	return detect.NewDetector(cfg), nil
}

// Scan inspects all added lines. When gitleaks is available, both gitleaks and the
// local fallback run. Findings with matching redacted values on the same line are
// deduplicated.
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
			local := s.scanLine(path, line)
			var gl []ports.SecretFinding
			if s.gitleaks != nil {
				gl = s.scanLineGitleaks(path, line)
			}
			findings = append(findings, dedupeFindings(local, gl)...)
		}
	}
	if len(findings) == 0 {
		return nil, nil
	}
	return findings, nil
}

// dedupeFindings merges findings and removes local findings that match the redacted text
// of any gitleaks findings. Gitleaks findings are preferred because their rule IDs
// are more specific and they account for allowlist context.
func dedupeFindings(local, gl []ports.SecretFinding) []ports.SecretFinding {
	if len(gl) == 0 {
		return local
	}
	seen := make(map[string]struct{}, len(gl))
	for _, f := range gl {
		seen[f.Redacted] = struct{}{}
	}
	out := append([]ports.SecretFinding(nil), gl...)
	for _, f := range local {
		if _, dup := seen[f.Redacted]; dup {
			continue
		}
		out = append(out, f)
	}
	return out
}

// scanLineGitleaks scans a single line using gitleaks. Scanning is performed
// line-by-line rather than across file fragments to maintain line-number mapping.
func (s *BuiltinScanner) scanLineGitleaks(path string, line ports.Line) []ports.SecretFinding {
	//lint:ignore SA1019 detect.Fragment deprecated; will migrate to sources.Fragment on gitleaks v9.
	frag := detect.Fragment{Raw: line.Text, FilePath: path} //nolint:staticcheck
	leaks := s.gitleaks.Detect(frag)
	if len(leaks) == 0 {
		return nil
	}
	out := make([]ports.SecretFinding, 0, len(leaks))
	for _, l := range leaks {
		if isDocsExampleSecret(l.Secret) {
			continue
		}
		out = append(out, ports.SecretFinding{
			Rule:       l.RuleID,
			FilePath:   path,
			Line:       line.Number,
			Redacted:   redactLine(line.Text, l.Secret),
			Confidence: 0.9,
		})
	}
	return out
}

func (s *BuiltinScanner) scanLine(path string, line ports.Line) []ports.SecretFinding {
	var findings []ports.SecretFinding
	matched := map[string]struct{}{}

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
		if isDocsExampleSecret(raw) {
			continue
		}
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
		// Skip tokens that look like typical programming language identifiers.
		// Long camelCase names can cross the entropy threshold but are not secrets.
		if looksLikeIdentifier(tok) {
			continue
		}
		// Skip tokens that match public Go import paths or package URLs to prevent
		// false positives on standard library dependencies.
		if looksLikeImportPath(tok) {
			continue
		}
		// Skip absolute Unix filesystem paths, as configuration files often reference
		// binary execution paths that exceed the entropy threshold.
		if looksLikeFilesystemPath(tok) {
			continue
		}
		// Skip HTTP and HTTPS URLs to avoid flagging documentation links, licenses,
		// and references in comments or markdown files.
		if looksLikeURL(tok) {
			continue
		}
		if isDocsExampleSecret(tok) {
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

// redactLine replaces the secret substring within the line with a masked string of
// equal length to keep the finding readable while obscuring the secret itself.
func redactLine(text, secret string) string {
	return strings.ReplaceAll(text, secret, mask(secret))
}

// mask returns a masked version of the secret. The first two characters are kept
// for context while the remaining characters are masked. Short secrets are fully masked.
func mask(secret string) string {
	if len(secret) <= 4 {
		return strings.Repeat("*", len(secret))
	}
	return secret[:2] + strings.Repeat("*", len(secret)-2)
}

// importPathRe matches common public Go import paths and module URLs.
// Private hostnames without dots are excluded to avoid accidentally bypassing secrets with slashes.
var importPathRe = regexp.MustCompile(`^(?:github\.com|gitlab\.com|bitbucket\.org|golang\.org|gopkg\.in|google\.golang\.org|cloud\.google\.com|k8s\.io|sigs\.k8s\.io|go\.uber\.org|go\.opentelemetry\.io)/[A-Za-z0-9._/\-]+$`)

func looksLikeImportPath(tok string) bool {
	return importPathRe.MatchString(tok)
}

// filesystemPathRe matches absolute Unix paths with at least three path components.
// Requiring multiple segments ensures single-slash secret tokens are not allowlisted.
var filesystemPathRe = regexp.MustCompile(`^/[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+){2,}$`)

func looksLikeFilesystemPath(tok string) bool {
	return filesystemPathRe.MatchString(tok)
}

// urlPathRe matches HTTP or HTTPS URL paths after the colon separator is stripped.
// Requiring a valid top-level domain suffix prevents general slash-prefixed tokens from matching.
var urlPathRe = regexp.MustCompile(`^//[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+(?:/[A-Za-z0-9._\-+/]*)?$`)

func looksLikeURL(tok string) bool {
	return urlPathRe.MatchString(tok)
}

// looksLikeIdentifier reports whether tok matches a programming language identifier
// or chain of identifiers. This excludes camelCase names, dot-accessed fields, and path-like
// parameters from being flagged as secrets. Real credentials typically contain digits,
// punctuation, or lacks letter runs, whereas identifiers start with a letter/underscore
// and contain a sequence of consecutive letters.
func looksLikeIdentifier(tok string) bool {
	if tok == "" {
		return false
	}
	for _, part := range identifierChainSeparators(tok) {
		if part == "" {
			continue
		}
		if !isIdentifierWord(part) {
			return false
		}
	}
	return hasLetterRun(tok, 3)
}

// identifierChainSeparators splits tok on '.', '/', and '-' separators.
func identifierChainSeparators(tok string) []string {
	parts := []string{}
	start := 0
	for i, r := range tok {
		if r == '.' || r == '/' || r == '-' {
			parts = append(parts, tok[start:i])
			start = i + len(string(r))
		}
	}
	parts = append(parts, tok[start:])
	return parts
}

// isIdentifierWord reports whether s matches the syntax of a single identifier word.
func isIdentifierWord(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// hasLetterRun reports whether s contains at least n consecutive ASCII letters.
// This is used to differentiate identifier chains from random alphanumeric secret strings.
func hasLetterRun(s string, n int) bool {
	run := 0
	for _, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if isLetter {
			run++
			if run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

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

