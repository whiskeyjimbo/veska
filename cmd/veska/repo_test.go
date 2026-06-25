// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoAddCmd_WaitWithoutDaemon covers: `repo add --wait`
// against an unreachable daemon used to silently fall back to direct-write
// (the flag became a no-op + a long warning line). The contract now is:
// wait + no daemon = explicit error naming `veska service start`, so the
// user knows the cold-scan they asked to wait on never actually started.
func TestRepoAddCmd_WaitWithoutDaemon(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	veskaHome := t.TempDir()
	t.Setenv("VESKA_HOME", veskaHome)

	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	cmd := newRootCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"repo", "add", repoDir, "--wait"})
	cmd.SetContext(context.Background())

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("repo add --wait must error when daemon is down; got stdout=%q", out.String())
	}
	msg := err.Error()
	for _, want := range []string{"daemon unreachable", "veska service start", "drop --wait"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing guidance %q", msg, want)
		}
	}
	// Critical: NO repo should have been written to the local DB on the
	// wait error path - otherwise re-running after `service start`
	// would hit "already registered" without ever cold-scanning.
	if _, statErr := exec.Command("test", "-f", filepath.Join(veskaHome, "veska.db")).Output(); statErr == nil {
		// DB exists (init.go path or other CLI ran earlier in this test).
		// Make sure no repo row was inserted by re-running `repo list` and
		// asserting empty output.
		listCmd := newRootCmd()
		var listOut bytes.Buffer
		listCmd.SetOut(&listOut)
		listCmd.SetErr(&listOut)
		listCmd.SetArgs([]string{"repo", "list"})
		listCmd.SetContext(context.Background())
		_ = listCmd.Execute()
		if strings.Contains(listOut.String(), repoDir) {
			t.Errorf("--wait error path must not register the repo; repo list contains %q", repoDir)
		}
	}
}

// TestRepoAddCmd_DirectFallback covers: when the daemon socket is
// unreachable (no daemon running for this VESKA_HOME), `veska repo add <path>`
// must still succeed by opening the local SQLite directly and calling
// repo.Add. The previous wiring passed db=nil and unconditionally printed
// "database not available", making the CLI subcommand dead code.
// We point VESKA_HOME at t.TempDir to keep the test hermetic - no daemon
// is running there so the dial fails fast and the fallback path executes.
func TestRepoAddCmd_DirectFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	veskaHome := t.TempDir()
	t.Setenv("VESKA_HOME", veskaHome)

	// Real git working tree (so the f8p branch-detection path is exercised
	// end-to-end: the CLI insert must record active_branch).
	repoDir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "T"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	cmd := newRootCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"repo", "add", repoDir})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("repo add: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "added repo ") {
		t.Errorf("stdout missing 'added repo': %q", out.String())
	}
	// Daemon offline → the fallback path must announce itself (so users know
	// the repo is registered but not yet scanned) and point at starting the
	// daemon. The raw dial error is logged at debug, not printed, so a
	// successful add reads as success rather than an alarm.
	o := out.String()
	if !strings.Contains(o, "daemon offline") {
		t.Errorf("stdout missing 'daemon offline' note: %q", o)
	}
	if !strings.Contains(o, "cold-scan") {
		t.Errorf("stdout missing cold-scan/start-daemon guidance: %q", o)
	}

	// Sanity: the DB exists at the expected path.
	if _, err := exec.Command("test", "-f", filepath.Join(veskaHome, "veska.db")).Output(); err != nil {
		t.Errorf("veska.db not created at %s", veskaHome)
	}
}
