package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRootVersionFlag pins: `veska --version` must print a
// compact one-liner instead of "unknown flag". Junior users reach for
// version before learning the `version` subcommand.
func TestRootVersionFlag(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--version: unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "veska ") {
		t.Errorf("--version output %q; want prefix \"veska \"", out)
	}
	if strings.Contains(out, "unknown flag") {
		t.Errorf("--version output contains \"unknown flag\": %q", out)
	}
}
