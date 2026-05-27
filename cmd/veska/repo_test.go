package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoAddCmd_WaitWithoutDaemon covers solov2-qnt9: `repo add --wait`
// against an unreachable daemon used to silently fall back to direct-write
// (the flag became a no-op + a long warning line). The contract now is:
// --wait + no daemon = explicit error naming `veska service start`, so the
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
	// --wait error path — otherwise re-running after `service start`
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

func TestPrintRepoTable_ShowsKindColumn(t *testing.T) {
	var buf bytes.Buffer
	printRepoTable(&buf, []repoView{
		{RepoID: "aaaaaaaaaaaaaaaaaa", RootPath: "/tmp/a", ActiveBranch: "main", LastPromotedSHA: "sha", Kind: "tracked"},
		{RepoID: "bbbbbbbbbbbbbbbbbb", RootPath: "/tmp/b", ActiveBranch: "main", Kind: "ephemeral"},
	})
	out := buf.String()

	// AC1: KIND column header between REPO_ID and BRANCH.
	if !strings.Contains(out, "REPO_ID") || !strings.Contains(out, "KIND") || !strings.Contains(out, "BRANCH") {
		t.Fatalf("missing expected headers in:\n%s", out)
	}
	idIdx := strings.Index(out, "REPO_ID")
	kindIdx := strings.Index(out, "KIND")
	branchIdx := strings.Index(out, "BRANCH")
	if !(idIdx < kindIdx && kindIdx < branchIdx) {
		t.Errorf("KIND must sit between REPO_ID and BRANCH; got header order:\n%s", out)
	}

	// Each row's kind value renders verbatim.
	if !strings.Contains(out, "tracked") {
		t.Errorf("missing 'tracked' value: %s", out)
	}
	if !strings.Contains(out, "ephemeral") {
		t.Errorf("missing 'ephemeral' value: %s", out)
	}
}

func TestPrintRepoTable_BlankKindDefaultsToTracked(t *testing.T) {
	var buf bytes.Buffer
	// A row with an empty Kind (e.g. from an older daemon response without
	// the field) must still render a value — never a blank cell.
	printRepoTable(&buf, []repoView{
		{RepoID: "aaaaaaaaaaaaaaaaaa", RootPath: "/tmp/a", ActiveBranch: "main", LastPromotedSHA: "sha"},
	})
	if !strings.Contains(buf.String(), "tracked") {
		t.Errorf("empty Kind should render as 'tracked', got:\n%s", buf.String())
	}
}

func TestPrintRepoTable_EmptyMessageUnchanged(t *testing.T) {
	var buf bytes.Buffer
	printRepoTable(&buf, nil)
	want := "no repositories registered — run: veska repo add <path>"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("missing empty-message %q; got:\n%s", want, buf.String())
	}
}
