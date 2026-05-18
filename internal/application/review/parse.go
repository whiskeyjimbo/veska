package review

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// modelFinding is the wire shape of one finding in a model's JSON response.
// It is decoded then validated and converted into a ReviewFinding.
type modelFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

// modelResponse is the wire shape of a review model's JSON response: an object
// with a "findings" array. An empty (or absent) array means the model found
// nothing — that is success, not an error.
type modelResponse struct {
	Findings []modelFinding `json:"findings"`
}

// parseJSON parses a model's structured JSON response into findings tagged
// with kind.
//
// The response contract: a JSON object with a "findings" array; each finding
// carries severity, title and message. Surrounding prose or whitespace is
// tolerated — the first balanced JSON object in the response is decoded. An
// empty findings array yields an empty slice and no error. Any response that
// cannot be decoded into the contract shape, or that carries an invalid
// finding, returns ErrMalformedResponse — the parser never panics.
func parseJSON(kind ReviewKind, modelOutput string) ([]ReviewFinding, error) {
	raw := extractJSONObject(modelOutput)
	if raw == "" {
		return nil, fmt.Errorf("%w: no JSON object in response", ErrMalformedResponse)
	}

	var resp modelResponse
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedResponse, err)
	}

	findings := make([]ReviewFinding, 0, len(resp.Findings))
	for i, mf := range resp.Findings {
		f, err := mf.toReviewFinding(kind)
		if err != nil {
			return nil, fmt.Errorf("%w: finding %d: %v", ErrMalformedResponse, i+1, err)
		}
		findings = append(findings, f)
	}
	return findings, nil
}

// toReviewFinding validates one decoded model finding and converts it into a
// ReviewFinding tagged with kind.
func (mf modelFinding) toReviewFinding(kind ReviewKind) (ReviewFinding, error) {
	title := strings.TrimSpace(mf.Title)
	if title == "" {
		return ReviewFinding{}, fmt.Errorf("missing title")
	}
	message := strings.TrimSpace(mf.Message)
	if message == "" {
		return ReviewFinding{}, fmt.Errorf("missing message")
	}
	sev, err := parseSeverity(mf.Severity)
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

// extractJSONObject returns the first balanced top-level JSON object in s,
// tolerating leading/trailing prose a real model often emits around it (e.g.
// "Here is the result:\n{...}\n"). It scans for the first '{' and tracks brace
// depth, skipping braces inside string literals. It returns an empty string
// when no object is present.
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// other chars inside a string are ignored
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
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
