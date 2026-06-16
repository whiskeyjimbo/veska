// Package secretsscanner provides the veska-builtin SecretsScanner: an
// in-process detector that runs gitleaks (~140 curated rules with rich
// allowlists for Go imports, URLs, lockfiles, etc.) and falls back to a
// small regex + Shannon-entropy heuristic when gitleaks initialization
// fails. Redaction is built in, so a raw secret value never reaches a
// finding. It is fast enough to run on every promotion.
// gitleaks replaces the entropy-only detector that produced
// false positives on Go import paths (subsumes ) and missed
// AWS Access Key shapes. Gitleaks' default config carries those exact
// allowlists/rules; the local regex+entropy path remains as a safety net
// so a broken gitleaks build/config never silently disables scanning.
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

// docsExampleSecrets lists well-known credential strings that vendors
// publish as literal placeholders in their own documentation. Flagging
// these creates a noise wall on the first-run journey — every junior
// who copy-pastes AWS's quickstart hits the same canonical key. Real
// callers never have a reason to ship these literal strings, so a
// strict-equality allowlist is safe.
var docsExampleSecrets = map[string]struct{}{
	// AWS canonical examples published throughout AWS docs and SDKs.
	"AKIAIOSFODNN7EXAMPLE":                     {},
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": {},
}

// isDocsExampleSecret reports whether raw is one of the canonical vendor
// documentation placeholders. Used by both detection paths to drop the
// finding before it reaches the output bucket.
func isDocsExampleSecret(raw string) bool {
	_, ok := docsExampleSecrets[raw]
	return ok
}

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
	strconv.Itoa(entropyMinLen) + `,}`)

// BuiltinScanner is the in-process SecretsScanner. It holds no mutable state
// after construction and is safe for concurrent use — gitleaks'
// detect.Detector is also concurrency-safe.
type BuiltinScanner struct {
	entropyThreshold float64
	// gitleaks is the primary detector when non-nil. nil means the
	// gitleaks rules failed to initialize at construction and the scanner
	// degrades to the local regex + entropy path.
	gitleaks *detect.Detector
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

// New constructs a BuiltinScanner. The default path initializes gitleaks
// with its bundled rule set; if init fails the scanner falls back to the
// local regex+entropy heuristic and logs a warning. It has no required
// external dependencies and never returns an error — secret scanning must
// not block promotion just because a rule file was malformed.
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

// newGitleaksDetector mirrors the reglet pattern (cross-checked at
// /home/jrose/src/all-reglet/./redactor.go): load gitleaks' embedded
// default config through viper, translate it, and build a Detector. The
// default config carries the allowlists we need — Go import paths, lockfiles,
// well-known package URLs — so we don't have to re-port them locally.
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

// Scan inspects every added line. Both detection paths run additively when
// gitleaks initialized — gitleaks catches novel shapes our local regex
// set misses (and skips known false-positive sources via its allowlists),
// while the local regex + entropy heuristic remains as a coarse safety
// net for unknown high-entropy tokens. Findings with the same redacted
// value on the same line are de-duped so callers don't see a double-hit
// for tokens both paths claim. A nil/empty input yields (nil, nil).
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

// dedupeFindings merges local and gitleaks findings for the same line,
// dropping local results whose Redacted text already appears in the
// gitleaks set. Gitleaks wins when it covers the same secret — its rule
// ID is more specific (e.g. "aws-access-token" vs our generic
// "aws-access-key-id") and it carries allowlist context.
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

// scanLineGitleaks runs the gitleaks detector against a single line and
// converts each gitleaks finding into our ports.SecretFinding shape. We
// run line-by-line (not file-fragments) to preserve the line-number
// contract callers rely on; gitleaks' Detect is O(rules × len(input))
// per call and is fast enough on single-line inputs.
func (s *BuiltinScanner) scanLineGitleaks(path string, line ports.Line) []ports.SecretFinding {
	//lint:ignore SA1019 detect.Fragment deprecated; will migrate to sources.Fragment on gitleaks v9.
	frag := detect.Fragment{Raw: line.Text, FilePath: path} //nolint:staticcheck // SA1019: see lint:ignore above
	leaks := s.gitleaks.Detect(frag)
	if len(leaks) == 0 {
		return nil
	}
	out := make([]ports.SecretFinding, 0, len(leaks))
	for _, l := range leaks {
		// drop canonical vendor-docs placeholders.
		if isDocsExampleSecret(l.Secret) {
			continue
		}
		out = append(out, ports.SecretFinding{
			Rule:       l.RuleID,
			FilePath:   path,
			Line:       line.Number,
			Redacted:   redactLine(line.Text, l.Secret),
			Confidence: 0.9, // gitleaks rules are high-precision by design.
		})
	}
	return out
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
		// drop canonical vendor-docs placeholders.
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
		// skip tokens that look like ordinary identifiers
		// (Go method/function/type names commonly cross the entropy
		// threshold when they're long camelCase strings). Real secrets
		// almost always include digits or punctuation; pure-alpha
		// identifiers are noise.
		if looksLikeIdentifier(tok) {
			continue
		}
		// (subsumed into ): Go import paths and
		// well-known module URLs cross the entropy threshold but are
		// never secrets. Gitleaks' default config carries the same
		// allowlists; we replicate the most common shape here so the
		// fallback path (when gitleaks init fails) does not regress.
		if looksLikeImportPath(tok) {
			continue
		}
		// Absolute Unix filesystem paths (multi-segment, path-shaped
		// components only) cross the entropy threshold but are never
		// secrets. Agent-config files like.mcp.json embed binary
		// paths (e.g. "/home/user/.local/bin/veska-mcp") that the
		// entropy heuristic otherwise flags — including the file
		// veska itself writes during `veska init --agent`.
		if looksLikeFilesystemPath(tok) {
			continue
		}
		// SPDX license URLs and other http(s) links in
		// comments / Markdown were producing 47 high-entropy false
		// positives on a fresh spf13/cobra clone. Skip URL-path-shaped
		// tokens BEFORE the entropy rule fires. A real secret on the
		// same line is unaffected — tokenRe captures it as a separate
		// token.
		if looksLikeURL(tok) {
			continue
		}
		// drop canonical vendor-docs placeholders.
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

// importPathRe matches a token that looks like a Go import path or
// module URL — host.tld/path/segments. Restricted to common public
// hosts plus generic <tld>/<path> shapes; private hostnames without
// dots intentionally do not match so true secrets containing slashes
// are not over-eagerly allowed.
var importPathRe = regexp.MustCompile(`^(?:github\.com|gitlab\.com|bitbucket\.org|golang\.org|gopkg\.in|google\.golang\.org|cloud\.google\.com|k8s\.io|sigs\.k8s\.io|go\.uber\.org|go\.opentelemetry\.io)/[A-Za-z0-9._/\-]+$`)

// looksLikeImportPath reports whether tok has the shape of a public
// Go import path / module URL.
func looksLikeImportPath(tok string) bool {
	return importPathRe.MatchString(tok)
}

// filesystemPathRe matches a token shaped like an absolute Unix
// filesystem path: leading slash, at least three path components,
// each component built only from path-safe characters (no random
// alphanumeric runs the way a secret would). Restricting to multi
// segment paths means a bare leading-slash token like "/sk_live_…"
// still trips the entropy rule.
var filesystemPathRe = regexp.MustCompile(`^/[A-Za-z0-9._\-]+(?:/[A-Za-z0-9._\-]+){2,}$`)

// looksLikeFilesystemPath reports whether tok has the shape of an
// absolute Unix filesystem path.
func looksLikeFilesystemPath(tok string) bool {
	return filesystemPathRe.MatchString(tok)
}

// urlPathRe matches tokens shaped like the path portion of an http(s)
// URL — after the colon has been stripped by tokenRe. Real-world
// shapes: "//www.apache.org/licenses/LICENSE-2.0",
// "//github.com/golang/go/issues/12345/",
// "//pkg.go.dev/github.com/spf13/cobra". Anchored: head is "//",
// followed by a hostname-like run (alphanumerics + dots + hyphens),
// then at least one dot + TLD-ish suffix, optionally followed by a
// path. The TLD requirement keeps the rule from allowlisting bare
// "//something" tokens that aren't URLs.
var urlPathRe = regexp.MustCompile(`^//[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+(?:/[A-Za-z0-9._\-+/]*)?$`)

// looksLikeURL reports whether tok has the shape of an http(s) URL
// after the scheme's colon has been stripped by tokenRe. SPDX license
// header URLs, README cross-references, and Markdown link targets are
// the dominant source — flagging them produced 47 high-entropy false
// positives on a fresh spf13/cobra clone.
func looksLikeURL(tok string) bool {
	return urlPathRe.MatchString(tok)
}

// looksLikeIdentifier reports whether tok has the shape of a programming
// language identifier or a dotted/slashed chain of identifiers. Three
// shapes are accepted, all of which routinely cross the high-entropy
// threshold on Go source and Markdown without being secrets:
//  1. Bare identifier: letters and underscores, no digits, no
//     punctuation (the original rule). Catches
//     "BenchmarkMemoryDuringPluginDiscovery".
//  2. Identifier with embedded digits: each separator-delimited word
//     starts with a letter or underscore, runs over [A-Za-z0-9_]
//     Catches "TestBashCompletionV2WithActiveHelp".
//  3. Dotted / slashed / hyphenated chain: each component matches
//     shape (2) above. Catches
//     "c.IsAdditionalHelpTopicCommand",
//     "mutuallyExclusive/oneRequired/requiredAsGroup",
//     "site/content/projects_using_cobra.md".
//
// Real credentials (base64, hex, random tokens) overwhelmingly contain
// digit-letter runs, '+', '=', or other punctuation NOT in the
// separator set; the identifier-shape check excludes those by
// requiring every component to start with a letter or underscore.
func looksLikeIdentifier(tok string) bool {
	if tok == "" {
		return false
	}
	// Per-component word-shape: every separator-delimited segment must
	// start with a letter or underscore and contain only [A-Za-z0-9_].
	// This rejects tokens with stray punctuation that real secrets carry
	// (base64 '+'/'=', random hex blocks separated by '-', etc.). Empty
	// parts come from leading/trailing separators (e.g. sentence-final
	// periods like "encoding.TextUnmarshaler." in a doc comment) and
	// must not disqualify an otherwise identifier-shaped token.
	for _, part := range identifierChainSeparators(tok) {
		if part == "" {
			continue
		}
		if !isIdentifierWord(part) {
			return false
		}
	}
	// Whole-token letter-run: at least one substring of three consecutive
	// letters anywhere in the token. This is what distinguishes real
	// identifier chains (Test.Bash.Completion has "Test","Bash",
	// "Completion") from random alphanumeric secrets ("h8Kq2Lx9Zp4Wn7…"
	// has no letter run > 2). Checked at token-grain so a short
	// receiver-like component (e.g. "c" in "c.IsAvailableCommand") does
	// not disqualify the whole chain.
	return hasLetterRun(tok, 3)
}

// identifierChainSeparators splits tok on '.', '/', and '-' — the three
// non-underscore separators that appear between Go identifiers in
// member-access (a.b), paths (a/b), and Markdown link slugs (a-b).
// Underscore is part of an identifier word, not a separator.
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

// isIdentifierWord reports whether s has the shape of a single
// identifier word: starts with a letter or underscore, then any run of
// letters/digits/underscores. The letter-run discriminator that
// distinguishes identifiers from random alphanumeric secrets is
// checked at token-grain by hasLetterRun — see looksLikeIdentifier.
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

// hasLetterRun reports whether s contains a substring of n consecutive
// ASCII letters anywhere. Used by looksLikeIdentifier to discriminate
// real identifier chains ("TestBashCompletionV2WithActiveHelp" — many
// long letter runs) from random alphanumeric secrets ("h8Kq2Lx9Zp4."
// no run > 2).
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
