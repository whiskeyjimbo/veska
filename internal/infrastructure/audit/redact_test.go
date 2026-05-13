package audit_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
	"github.com/whiskeyjimbo/engram/solov2/internal/core/ports"
	"github.com/whiskeyjimbo/engram/solov2/internal/infrastructure/audit"
)

// fakeWriter captures the last AuditEntry written to it.
type fakeWriter struct {
	last ports.AuditEntry
}

func (f *fakeWriter) Write(_ context.Context, e ports.AuditEntry) error {
	f.last = e
	return nil
}

func TestRedactAPIKeys(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"sk-abc123", "[REDACTED]"},
		{"key is sk-abc123 here", "key is [REDACTED] here"},
		{"ghp_foobar", "[REDACTED]"},
		{"token: ghp_foobar!", "token: [REDACTED]!"},
		{"xoxb-token", "[REDACTED]"},
		{"slack xoxb-token now", "slack [REDACTED] now"},
		{"Bearer mytoken", "Bearer [REDACTED]"},
		{"Authorization: Bearer mytoken123", "Authorization: Bearer [REDACTED]"},
	}
	for _, tc := range cases {
		got := audit.Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestRedactURLPasswords(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			"postgres://user:s3cr3t@host/db",
			"postgres://user:[REDACTED]@host/db",
		},
		{
			"mysql://admin:P@ssw0rd!@localhost:3306/mydb",
			"mysql://admin:[REDACTED]@localhost:3306/mydb",
		},
		{
			"connecting to redis://default:topsecret@cache.example.com",
			"connecting to redis://default:[REDACTED]@cache.example.com",
		},
	}
	for _, tc := range cases {
		got := audit.Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestRedactEnvAssignments(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"API_KEY=abc123", "API_KEY=[REDACTED]"},
		{"TOKEN=xyz", "TOKEN=[REDACTED]"},
		{"SECRET=my-secret-value", "SECRET=[REDACTED]"},
		{"PASSWORD=pass", "PASSWORD=[REDACTED]"},
		{"PASSWD=pw", "PASSWD=[REDACTED]"},
		{"export API_KEY=abc123", "export API_KEY=[REDACTED]"},
		{"DB_PASSWORD=hunter2 other=stuff", "DB_PASSWORD=[REDACTED] other=stuff"},
	}
	for _, tc := range cases {
		got := audit.Redact(tc.input)
		if got != tc.want {
			t.Errorf("Redact(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestRedactPassthrough(t *testing.T) {
	cases := []string{
		"node.save completed successfully",
		"branch: main",
		"repo-id: abc-123",
		"user logged in from 192.168.1.1",
		"file path /home/user/project/main.go",
		"error: file not found",
		"https://example.com/path/to/resource",
		"postgres://user@host/db", // no password in URL
		"USER=alice",              // USER is not a secret name
		"VARIABLE=somevalue",      // not a known secret name
		"token_count=42",          // not a TOKEN= assignment
	}
	for _, tc := range cases {
		got := audit.Redact(tc)
		if got != tc {
			t.Errorf("Redact(%q) modified non-secret string: got %q", tc, got)
		}
	}
}

func TestRedactingWriter(t *testing.T) {
	inner := &fakeWriter{}
	w := audit.NewRedactingWriter(inner)

	e := ports.AuditEntry{
		RepoID:    "repo-1",
		ActorID:   "human:alice",
		ActorKind: domain.ActorKindHuman,
		Op:        "node.save",
		TargetID:  "sk-abc123",
		Branch:    "main",
		CreatedAt: time.Now().UTC(),
	}

	if err := w.Write(context.Background(), e); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if inner.last.TargetID != "[REDACTED]" {
		t.Errorf("TargetID not redacted: got %q, want [REDACTED]", inner.last.TargetID)
	}
	// Non-secret fields should pass through unchanged.
	if inner.last.RepoID != "repo-1" {
		t.Errorf("RepoID was unexpectedly modified: got %q", inner.last.RepoID)
	}
	if inner.last.Op != "node.save" {
		t.Errorf("Op was unexpectedly modified: got %q", inner.last.Op)
	}
}

func TestRedactingWriterAllFields(t *testing.T) {
	inner := &fakeWriter{}
	w := audit.NewRedactingWriter(inner)

	e := ports.AuditEntry{
		RepoID:    "API_KEY=mysecret",
		ActorID:   "Bearer secrettoken",
		ActorKind: domain.ActorKindHuman,
		Op:        "node.save",
		TargetID:  "ghp_foobar",
		Branch:    "TOKEN=xyz",
		CreatedAt: time.Now().UTC(),
	}

	if err := w.Write(context.Background(), e); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if inner.last.RepoID != "API_KEY=[REDACTED]" {
		t.Errorf("RepoID: got %q", inner.last.RepoID)
	}
	if inner.last.ActorID != "Bearer [REDACTED]" {
		t.Errorf("ActorID: got %q", inner.last.ActorID)
	}
	if inner.last.TargetID != "[REDACTED]" {
		t.Errorf("TargetID: got %q", inner.last.TargetID)
	}
	if inner.last.Branch != "TOKEN=[REDACTED]" {
		t.Errorf("Branch: got %q", inner.last.Branch)
	}
}

func TestRedactFile(t *testing.T) {
	input := []byte("" +
		"repo_id: repo-1\n" +
		"api_key: sk-abc123secret\n" +
		"branch: main\n" +
		"token: ghp_foobar\n" +
		"normal: no secrets here\n",
	)

	got := audit.RedactFile(input)

	lines := bytes.Split(got, []byte("\n"))
	// lines[0] = "repo_id: repo-1"        — unchanged
	// lines[1] = "api_key: sk-..."        — redacted
	// lines[2] = "branch: main"           — unchanged
	// lines[3] = "token: ghp_foobar"      — redacted
	// lines[4] = "normal: no secrets..."  — unchanged
	// lines[5] = ""                        — trailing newline

	if !bytes.Equal(lines[0], []byte("repo_id: repo-1")) {
		t.Errorf("line 0 unexpectedly changed: %q", lines[0])
	}
	if bytes.Contains(lines[1], []byte("sk-abc123secret")) {
		t.Errorf("line 1 still contains secret: %q", lines[1])
	}
	if !bytes.Equal(lines[2], []byte("branch: main")) {
		t.Errorf("line 2 unexpectedly changed: %q", lines[2])
	}
	if bytes.Contains(lines[3], []byte("ghp_foobar")) {
		t.Errorf("line 3 still contains secret: %q", lines[3])
	}
	if !bytes.Equal(lines[4], []byte("normal: no secrets here")) {
		t.Errorf("line 4 unexpectedly changed: %q", lines[4])
	}
}
