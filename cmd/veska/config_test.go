// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestConfigShowOutputsTOML pins: `veska config show` must
// emit the resolved config in a parseable shape (TOML by default, JSON
// with --json). The body matters less than the contract: it succeeds,
// writes to stdout, and surfaces at least one well-known top-level key.
func TestConfigShowOutputsTOML(t *testing.T) {
	cmd := configShowCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("config show: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"[daemon]", "[storage]", "[embedder]"} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output missing %q; got:\n%s", want, out)
		}
	}
}

// TestConfigShowJSON pins the --json branch.
func TestConfigShowJSON(t *testing.T) {
	cmd := configShowCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("config show --json: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("config show --json should emit a JSON object; got:\n%s", out)
	}
}
