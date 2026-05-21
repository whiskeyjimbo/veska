package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoAddCmd_DirectFallback covers solov2-trh: when the daemon socket is
// unreachable (no daemon running for this VESKA_HOME), `veska repo add <path>`
// must still succeed by opening the local SQLite directly and calling
// repo.Add. The previous wiring passed db=nil and unconditionally printed
// "database not available", making the CLI subcommand dead code.
//
// We point VESKA_HOME at t.TempDir() to keep the test hermetic — no daemon
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
	// Daemon offline → fallback path must announce itself so users know
	// the daemon-restart caveat applies. Post-0cg the message now includes
	// the underlying dial error so users can tell 'daemon down' from
	// 'daemon up but unreachable'.
	o := out.String()
	if !strings.Contains(o, "daemon dial failed") {
		t.Errorf("stdout missing 'daemon dial failed' note: %q", o)
	}
	if !strings.Contains(o, "restart daemon") {
		t.Errorf("stdout missing 'restart daemon' guidance: %q", o)
	}

	// Sanity: the DB exists at the expected path.
	if _, err := exec.Command("test", "-f", filepath.Join(veskaHome, "veska.db")).Output(); err != nil {
		t.Errorf("veska.db not created at %s", veskaHome)
	}
}
