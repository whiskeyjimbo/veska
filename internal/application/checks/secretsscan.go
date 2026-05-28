package checks

import (
	"context"
	"fmt"
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

	out := make([]*domain.Finding, 0, len(secrets))
	for _, s := range secrets {
		msg := fmt.Sprintf("secret detected by rule %q at line %d: %s", s.Rule, s.Line, s.Redacted)
		f, err := domain.NewFinding(
			in.RepoID, in.Branch,
			secretSeverity(s.Confidence),
			domain.LayerSecurity,
			"secret_leak",
			msg,
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
// auto-link rules — see pathfilter.IsVendored.
func isSecretsScanIgnored(path string) bool {
	for _, prefix := range secretsScanIgnoredPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return pathfilter.IsVendored(path)
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
