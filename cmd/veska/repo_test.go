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

// TestColdScanRunningHint covers solov2-rhaq: the post-`repo add` hint must
// attribute `--wait` to `veska repo add`, not to `veska repo list`. A junior
// reading the message will naturally copy-paste; suggesting `repo list --wait`
// (the prior wording) errors with "unknown flag". The corrected hint embeds
// the actual <path> so the suggestion is copy-pasteable.
func TestColdScanRunningHint(t *testing.T) {
	root := "/some/repo/path"
	logPath := "/tmp/veska/logs/daemon.log"
	msg := coldScanRunningHint(root, logPath)

	// Must suggest --wait on `repo add <path>`, with the actual path inlined.
	wantAdd := "veska repo add " + root + " --wait"
	if !strings.Contains(msg, wantAdd) {
		t.Errorf("hint should suggest %q; got %q", wantAdd, msg)
	}

	// Must NOT suggest `repo list --wait` (the bug we are fixing). Detect the
	// trap precisely: a backtick-quoted command starting with "veska repo list"
	// and containing --wait, or a bare "repo list --wait".
	if strings.Contains(msg, "repo list --wait") || strings.Contains(msg, "repo list` to block") {
		t.Errorf("hint must not attribute --wait to `repo list`: %q", msg)
	}

	// Must still mention `veska repo list` (for status) and the log path.
	if !strings.Contains(msg, "veska repo list") {
		t.Errorf("hint should mention `veska repo list` for status: %q", msg)
	}
	if !strings.Contains(msg, logPath) {
		t.Errorf("hint should include log path %q: %q", logPath, msg)
	}
}
