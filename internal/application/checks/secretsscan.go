package checks

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// secretsScanIgnoredPrefixes are file-path prefixes the secrets scanner skips
// by default. These name on-disk caches for issue trackers and similar tools
// whose payloads routinely contain high-entropy slugs (memory keys, sequence
// hashes) that are not credentials. Without this filter the high-entropy
// heuristic fires repeatedly on the project's own tracker file — exactly the
// failure mode that hit on .beads/issues.jsonl during junior-journey round 3
// (solov2-jtl5.3). Paths are matched as prefixes against the promotion's
// repo-relative file path.
var secretsScanIgnoredPrefixes = []string{
	".beads/", // beads issue tracker (https://github.com/steveyegge/beads)
	".bd/",    // legacy beads layout
}

// SecretsScanCheck is a structural check that turns SecretsScanner output into
// findings on promotion. It scans only the lines newly added by the promoted
// commit (Input.AddedLines), so a pre-existing secret on an untouched line is
// excluded by construction — no extra filtering is required.
//
// Findings anchor on the leaking file's path with a discriminator key of
// rule+line, which makes the resulting finding_ids branch-stable and
// idempotent: re-running on unchanged input yields byte-identical finding_ids.
// The matched rule name and the scanner's Redacted snippet are surfaced in the
// finding Message; the raw secret is never read or stored by this check.
type SecretsScanCheck struct {
	scanner ports.SecretsScanner
}

// NewSecretsScanCheck constructs a SecretsScanCheck bound to a SecretsScanner.
// The scanner is required; passing nil will cause Run to return an error on
// first invocation.
func NewSecretsScanCheck(scanner ports.SecretsScanner) *SecretsScanCheck {
	return &SecretsScanCheck{scanner: scanner}
}

// Name returns the Prometheus / finding-rule attribution name.
func (c *SecretsScanCheck) Name() string { return "secrets-scan" }

// Run scans the promotion's newly-added lines for secret-shaped values and
// emits one finding per detected secret. When no lines were added it is a
// no-op returning (nil, nil).
func (c *SecretsScanCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.scanner == nil {
		return nil, fmt.Errorf("secrets-scan: nil dependency")
	}
	if len(in.AddedLines) == 0 {
		return nil, nil
	}

	scanInput := ports.ScanInput{AddedLines: make(map[string][]ports.Line, len(in.AddedLines))}
	for path, lines := range in.AddedLines {
		if isSecretsScanIgnored(path) {
			continue
		}
		converted := make([]ports.Line, len(lines))
		for i, l := range lines {
			converted[i] = ports.Line{Number: l.Number, Text: l.Text}
		}
		scanInput.AddedLines[path] = converted
	}
	if len(scanInput.AddedLines) == 0 {
		return nil, nil
	}

	secrets, err := c.scanner.Scan(scanInput)
	if err != nil {
		return nil, fmt.Errorf("secrets-scan: scan: %w", err)
	}

	// solov2-izh6.13: collapse N consecutive same-rule hits on the same
	// file into a single finding. A 28-line PEM block flagged line-by-line
	// otherwise produces 28 separate findings and dominates the surface on
	// `veska search --repo <url>` of e.g. jwt-go. The keyed first-occurrence
	// line is retained so the finding still anchors to a real position; the
	// count is surfaced in the message.
	type fileRuleKey struct{ file, rule string }
	type collapsed struct {
		first ports.SecretFinding
		count int
	}
	bucket := make(map[fileRuleKey]*collapsed, len(secrets))
	order := make([]fileRuleKey, 0, len(secrets))
	for _, s := range secrets {
		k := fileRuleKey{file: s.FilePath, rule: s.Rule}
		if cur, ok := bucket[k]; ok {
			cur.count++
			if s.Line < cur.first.Line {
				cur.first = s
			}
			continue
		}
		bucket[k] = &collapsed{first: s, count: 1}
		order = append(order, k)
	}

	out := make([]*domain.Finding, 0, len(order))
	for _, k := range order {
		c := bucket[k]
		s := c.first
		var msg string
		if c.count > 1 {
			msg = fmt.Sprintf("secret detected by rule %q at line %d (+%d more line(s) in same file): %s",
				s.Rule, s.Line, c.count-1, s.Redacted)
		} else {
			msg = fmt.Sprintf("secret detected by rule %q at line %d: %s", s.Rule, s.Line, s.Redacted)
		}
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID:   in.RepoID,
			Branch:   in.Branch,
			Severity: secretSeverity(s.Confidence),
			Layer:    domain.LayerSecurity,
			Rule:     "secret_leak",
			Message:  msg,
		},
			domain.WithFileAnchor(s.FilePath),
			domain.WithFindingKey(s.Rule+strconv.Itoa(s.Line)),
		)
		if err != nil {
			// A malformed scanner result should not abort the whole check.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// isSecretsScanIgnored reports whether path lives under a tracker/cache
// directory or a dependency-vendoring directory whose payloads routinely look
// secret-shaped to the high-entropy heuristic but never actually contain
// credentials. The vendored-deps half is shared with the dead-code and
// auto-link rules — see pathfilter.IsVendored. Test-fixture credential
// extensions under conventional test directories are also skipped, since
// PEM/key files inside testdata/ or test/ subtrees are vanishingly unlikely
// to be real and dominate the noise on forks of crypto-adjacent projects
// like jwt-go (solov2-izh6.13).
func isSecretsScanIgnored(path string) bool {
	for _, prefix := range secretsScanIgnoredPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if pathfilter.IsVendored(path) {
		return true
	}
	return isTestFixtureCredentialPath(path)
}

// testFixtureSegments are path segments that mark a directory subtree as
// test fixtures. Match is segment-aware (slash-bounded) so substring
// collisions like "contestdata/" or "testify_helper.go" do not trigger.
var testFixtureSegments = []string{"testdata", "test", "tests", "fixtures"}

// testFixtureCredentialExts are file extensions whose appearance under a
// test-fixture subtree is treated as a fake key for the purpose of the
// secrets scanner. Real production keys with these extensions live under
// config/, deploy/, etc., which the segment filter leaves untouched.
var testFixtureCredentialExts = []string{".pem", ".key", ".crt", ".cer", ".p12", ".pfx"}

// isTestFixtureCredentialPath reports whether path looks like a credential
// file embedded as a test fixture: a credential-shaped extension AND at
// least one path segment indicating a test directory. Matching only on
// segments avoids the "vendored_data/" / "contestdata/" substring trap that
// already bit the vendored-deps filter .
func isTestFixtureCredentialPath(path string) bool {
	lower := strings.ToLower(path)
	extMatch := false
	for _, ext := range testFixtureCredentialExts {
		if strings.HasSuffix(lower, ext) {
			extMatch = true
			break
		}
	}
	if !extMatch {
		return false
	}
	segments := strings.SplitSeq(lower, "/")
	for seg := range segments {
		if slices.Contains(testFixtureSegments, seg) {
			return true
		}
	}
	return false
}

// secretSeverity maps a scanner confidence score onto the domain Severity
// enum. A leaked secret is always serious, so the floor is High; a
// high-confidence match is escalated to Critical.
func secretSeverity(confidence float64) domain.Severity {
	if confidence >= 0.9 {
		return domain.SeverityCritical
	}
	return domain.SeverityHigh
}
