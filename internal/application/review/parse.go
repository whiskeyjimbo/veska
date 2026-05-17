package review

import (
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// noFindingsSentinel is the exact reply a prompt instructs the model to send
// when it found nothing. Matched case-insensitively after trimming.
const noFindingsSentinel = "NO FINDINGS"

// blockDelimiter separates finding blocks in a model response: a line whose
// trimmed content is exactly "---".
const blockDelimiter = "---"

// parseBlocks parses a model response in the package's block format into
// structured findings tagged with kind.
//
// The response contract: zero or more blocks separated by a delimiter line;
// each block carries SEVERITY, TITLE and MESSAGE lines. A response equal to
// the no-findings sentinel yields an empty slice and no error. Any block the
// parser cannot interpret (missing field, unknown severity) returns
// ErrMalformedResponse — the parser never panics.
func parseBlocks(kind ReviewKind, modelOutput string) ([]ReviewFinding, error) {
	trimmed := strings.TrimSpace(modelOutput)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty response", ErrMalformedResponse)
	}
	if strings.EqualFold(trimmed, noFindingsSentinel) {
		return []ReviewFinding{}, nil
	}

	blocks := splitBlocks(trimmed)
	findings := make([]ReviewFinding, 0, len(blocks))
	for i, block := range blocks {
		f, err := parseBlock(kind, block)
		if err != nil {
			return nil, fmt.Errorf("%w: block %d: %v", ErrMalformedResponse, i+1, err)
		}
		findings = append(findings, f)
	}
	if len(findings) == 0 {
		return nil, fmt.Errorf("%w: no finding blocks and not the no-findings sentinel", ErrMalformedResponse)
	}
	return findings, nil
}

// splitBlocks splits a response on delimiter-only lines, dropping blocks that
// are entirely blank.
func splitBlocks(s string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		joined := strings.TrimSpace(strings.Join(cur, "\n"))
		if joined != "" {
			blocks = append(blocks, joined)
		}
		cur = cur[:0]
	}
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) == blockDelimiter {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// parseBlock parses one finding block. Field lines are matched by an
// uppercase "KEY:" prefix; a MESSAGE may span lines and absorbs any trailing
// continuation lines.
func parseBlock(kind ReviewKind, block string) (ReviewFinding, error) {
	var severity, title string
	var messageParts []string
	inMessage := false

	for line := range strings.SplitSeq(block, "\n") {
		switch {
		case strings.HasPrefix(line, "SEVERITY:"):
			severity = strings.TrimSpace(strings.TrimPrefix(line, "SEVERITY:"))
			inMessage = false
		case strings.HasPrefix(line, "TITLE:"):
			title = strings.TrimSpace(strings.TrimPrefix(line, "TITLE:"))
			inMessage = false
		case strings.HasPrefix(line, "MESSAGE:"):
			messageParts = append(messageParts, strings.TrimSpace(strings.TrimPrefix(line, "MESSAGE:")))
			inMessage = true
		case inMessage:
			messageParts = append(messageParts, line)
		}
	}

	if severity == "" {
		return ReviewFinding{}, fmt.Errorf("missing SEVERITY field")
	}
	if title == "" {
		return ReviewFinding{}, fmt.Errorf("missing TITLE field")
	}
	message := strings.TrimSpace(strings.Join(messageParts, "\n"))
	if message == "" {
		return ReviewFinding{}, fmt.Errorf("missing MESSAGE field")
	}

	sev, err := parseSeverity(severity)
	if err != nil {
		return ReviewFinding{}, err
	}
	return ReviewFinding{
		Title:    title,
		Message:  message,
		Severity: sev,
		Kind:     kind,
	}, nil
}

// parseSeverity validates a severity token against the domain severity enum.
func parseSeverity(s string) (domain.Severity, error) {
	sev := domain.Severity(strings.ToLower(strings.TrimSpace(s)))
	switch sev {
	case domain.SeverityInfo, domain.SeverityLow, domain.SeverityMedium,
		domain.SeverityHigh, domain.SeverityCritical:
		return sev, nil
	default:
		return "", fmt.Errorf("invalid severity %q", s)
	}
}
