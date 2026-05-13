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

// TestDoctorStubSubcommandsExitZero verifies stub subcommands (those with no real probe yet)
// run without error and exit 0.
func TestDoctorStubSubcommandsExitZero(t *testing.T) {
	// These are pure stubs that always return ok.
	stubNames := []string{
		"pipelines",
		"bundle",
	}

	for _, name := range stubNames {
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

// TestDoctorStorageExitZero verifies the storage subcommand runs without error
// (it only reads the filesystem and does not require live services).
func TestDoctorStorageExitZero(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "storage"})
	if err := root.Execute(); err != nil {
		t.Errorf("subcommand storage: expected exit 0, got error: %v", err)
	}
}

// TestDoctorRealProbeSubcommandsRun verifies the live-probe subcommands (embedder, egress,
// config, status) execute without panicking.  In a test environment the probes may return
// degraded/broken but they must not crash or return an unexpected Go error.
func TestDoctorRealProbeSubcommandsRun(t *testing.T) {
	probeNames := []string{"embedder", "egress", "config", "status"}

	for _, name := range probeNames {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"doctor", name})
			// We do NOT assert err == nil — probes may legitimately return a
			// ProbeStatusError when services are unavailable in CI.  We only
			// assert the command ran (no panic) and any error is a ProbeStatusError.
			err := root.Execute()
			if err != nil {
				var pse ProbeStatusError
				if !isProbeStatusError(err, &pse) {
					t.Errorf("subcommand %q: unexpected non-probe error: %v", name, err)
				}
			}
		})
	}
}

// TestDoctorHelp verifies `veska doctor --help` lists subcommands.
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
