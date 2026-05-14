package main

import (
	"bytes"
	"testing"
)

// TestDoctorCmdName verifies doctorCmd returns a command named "doctor".
func TestDoctorCmdName(t *testing.T) {
	cmd := doctorCmd()
	if cmd.Use != "doctor" {
		t.Errorf("expected Use=doctor, got %q", cmd.Use)
	}
}

// TestDoctorSubcommands verifies all 8 required subcommands are registered.
func TestDoctorSubcommands(t *testing.T) {
	want := []string{
		"status",
		"egress",
		"storage",
		"embedder",
		"config",
		"post_promotion_queue",
		"pipelines",
		"bundle",
	}

	cmd := doctorCmd()
	found := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		found[sub.Use] = true
	}

	for _, name := range want {
		if !found[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

// TestDoctorSubcommandsHaveJSONFlag verifies each subcommand has a --json flag.
func TestDoctorSubcommandsHaveJSONFlag(t *testing.T) {
	cmd := doctorCmd()
	for _, sub := range cmd.Commands() {
		f := sub.Flags().Lookup("json")
		if f == nil {
			t.Errorf("subcommand %q missing --json flag", sub.Use)
		}
	}
}

// TestDoctorSubcommandsExitZero verifies each subcommand runs without error (stub exit-0).
func TestDoctorSubcommandsExitZero(t *testing.T) {
	subNames := []string{
		"status",
		"egress",
		"storage",
		"embedder",
		"config",
		"post_promotion_queue",
		"pipelines",
		"bundle",
	}

	for _, name := range subNames {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"doctor", name})
			if err := root.Execute(); err != nil {
				t.Errorf("subcommand %q: expected exit 0, got error: %v", name, err)
			}
		})
	}
}

// TestDoctorHelp verifies `engram doctor --help` lists subcommands.
func TestDoctorHelp(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor --help failed: %v", err)
	}
	help := out.String()
	for _, sub := range []string{"status", "egress", "storage", "embedder", "config", "post_promotion_queue", "pipelines", "bundle"} {
		if !bytes.Contains([]byte(help), []byte(sub)) {
			t.Errorf("--help output missing subcommand %q", sub)
		}
	}
}
