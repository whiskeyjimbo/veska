package audit

import (
	"bytes"
	"context"
	"regexp"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// redacted is the replacement text for any matched secret value.
const redacted = "[REDACTED]"

// Package-level compiled regexes are read-only after init, so they are safe
// for concurrent use from multiple goroutines without any locking.
var (
	// reAPIKey matches common API key prefixes followed by non-whitespace characters.
	// Groups: sk-..., ghp_..., xoxb-...
	reAPIKey = regexp.MustCompile(`(?:sk-|ghp_|xoxb-)[\w\-]+`)

	// reBearer matches a Bearer token in an Authorization header or similar context.
	// Captures everything after "Bearer " up to the next whitespace or end of string.
	reBearer = regexp.MustCompile(`(Bearer )\S+`)

	// reURLPassword matches the password portion of a URL with userinfo (user:pass@host).
	// The full userinfo ://user:password@host is captured so we can replace only the
	// password.  We match ://[^/\s]+@ greedily (consuming the entire userinfo including
	// any @ inside the password), then split on the first colon after ://.
	reURLPassword = regexp.MustCompile(`(://[^:/\s]+:)[^/\s]+(@)`)

	// reEnvSecret matches environment-variable-style assignments whose names contain
	// known secret keywords. The name must end with (or equal) one of the keywords.
	// Value is everything after the = up to whitespace or end of string.
	reEnvSecret = regexp.MustCompile(`(?i)(\b(?:\w*_)?(?:API_KEY|TOKEN|SECRET|PASSWORD|PASSWD)=)\S+`)
)

// Redact replaces patterns that look like secrets within s with [REDACTED].
// Non-secret content is returned unchanged. The function is safe for concurrent use.
func Redact(s string) string {
	s = reAPIKey.ReplaceAllString(s, redacted)
	s = reBearer.ReplaceAllString(s, "${1}"+redacted)
	s = reURLPassword.ReplaceAllString(s, "${1}"+redacted+"${2}")
	s = reEnvSecret.ReplaceAllString(s, "${1}"+redacted)
	return s
}

// RedactFile redacts each line of src independently and returns the result.
// It is intended for processing config files and audit.jsonl tails in doctor bundles.
func RedactFile(src []byte) []byte {
	lines := bytes.Split(src, []byte("\n"))
	for i, line := range lines {
		lines[i] = []byte(Redact(string(line)))
	}
	return bytes.Join(lines, []byte("\n"))
}

// RedactingAuditWriter wraps an inner AuditWriter and redacts all string fields
// of every AuditEntry before delegating to the inner writer. It implements the
// ports.AuditWriter interface and is safe for concurrent use.
type RedactingAuditWriter struct {
	inner ports.AuditWriter
}

// NewRedactingWriter returns a RedactingAuditWriter that wraps inner.
func NewRedactingWriter(inner ports.AuditWriter) *RedactingAuditWriter {
	return &RedactingAuditWriter{inner: inner}
}

// Write redacts all string fields of e, then delegates to the inner writer.
// It does not mutate the caller's entry — a copy is made before redaction.
func (w *RedactingAuditWriter) Write(ctx context.Context, e ports.AuditEntry) error {
	e.RepoID = Redact(e.RepoID)
	e.ActorID = Redact(e.ActorID)
	e.Op = Redact(e.Op)
	e.TargetID = Redact(e.TargetID)
	e.Branch = Redact(e.Branch)
	return w.inner.Write(ctx, e)
}

// Compile-time assertion: *RedactingAuditWriter implements ports.AuditWriter.
var _ ports.AuditWriter = (*RedactingAuditWriter)(nil)
