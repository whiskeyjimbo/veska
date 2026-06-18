// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package audit

import (
	"bytes"
	"context"
	"regexp"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// redacted is the replacement text for any matched secret value.
const redacted = "[REDACTED]"

// Package-level compiled regular expressions are read-only after init, so they
// are safe for concurrent use from multiple goroutines without locking.
var (
	// reAPIKey matches common API key prefixes followed by non-whitespace characters.
	reAPIKey = regexp.MustCompile(`(?:sk-|ghp_|xoxb-)[\w\-]+`)

	// reBearer matches a Bearer token in an Authorization header.
	reBearer = regexp.MustCompile(`(Bearer )\S+`)

	// reURLPassword matches the password portion of a URL. It consumes the
	// entire userinfo including any @ inside the password, then splits on the
	// first colon.
	reURLPassword = regexp.MustCompile(`(://[^:/\s]+:)[^/\s]+(@)`)

	// reEnvSecret matches environment-variable-style assignments whose names contain
	// known secret keywords.
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
func RedactFile(src []byte) []byte {
	lines := bytes.Split(src, []byte("\n"))
	for i, line := range lines {
		lines[i] = []byte(Redact(string(line)))
	}
	return bytes.Join(lines, []byte("\n"))
}

// RedactingAuditWriter wraps an inner AuditWriter and redacts all string fields
// of every AuditEntry before delegating to the inner writer. It is safe for
// concurrent use.
type RedactingAuditWriter struct {
	inner ports.AuditWriter
}

// NewRedactingWriter returns a RedactingAuditWriter that wraps inner.
func NewRedactingWriter(inner ports.AuditWriter) *RedactingAuditWriter {
	return &RedactingAuditWriter{inner: inner}
}

// Write redacts all string fields of e, then delegates to the inner writer.
func (w *RedactingAuditWriter) Write(ctx context.Context, e ports.AuditEntry) error {
	e.RepoID = Redact(e.RepoID)
	e.ActorID = Redact(e.ActorID)
	e.Op = Redact(e.Op)
	e.TargetID = Redact(e.TargetID)
	e.Branch = Redact(e.Branch)
	e.Reason = Redact(e.Reason)
	return w.inner.Write(ctx, e)
}

// Compile-time assertion: *RedactingAuditWriter implements ports.AuditWriter.
var _ ports.AuditWriter = (*RedactingAuditWriter)(nil)
