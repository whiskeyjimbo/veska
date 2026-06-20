// SPDX-License-Identifier: AGPL-3.0-only

package repocmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestColdScanRunningHint covers: the post-`repo add` hint must
// attribute `--wait` to `veska repo add`, not to `veska repo list`. A junior
// reading the message will naturally copy-paste; suggesting `repo list --wait`
// (the prior wording) errors with "unknown flag". The corrected hint embeds
// the actual <path> so the suggestion is copy-pasteable.
func TestColdScanRunningHint(t *testing.T) {
	root := "/some/repo/path"
	logPath := "/tmp/veska/logs/daemon.log"
	msg := ColdScanRunningHint(root, logPath)

	wantAdd := "veska repo add " + root + " --wait"
	if !strings.Contains(msg, wantAdd) {
		t.Errorf("hint should suggest %q; got %q", wantAdd, msg)
	}
	if strings.Contains(msg, "repo list --wait") || strings.Contains(msg, "repo list` to block") {
		t.Errorf("hint must not attribute --wait to `repo list`: %q", msg)
	}
	if !strings.Contains(msg, "veska repo list") {
		t.Errorf("hint should mention `veska repo list` for status: %q", msg)
	}
	if !strings.Contains(msg, logPath) {
		t.Errorf("hint should include log path %q: %q", logPath, msg)
	}
}

func TestPrintRepoTable_ShowsKindColumn(t *testing.T) {
	var buf bytes.Buffer
	PrintRepoTable(&buf, []RepoView{
		{RepoID: "aaaaaaaaaaaaaaaaaa", RootPath: "/tmp/a", ActiveBranch: "main", LastPromotedSHA: "sha", Kind: "tracked"},
		{RepoID: "bbbbbbbbbbbbbbbbbb", RootPath: "/tmp/b", ActiveBranch: "main", Kind: "ephemeral"},
	})
	out := buf.String()

	if !strings.Contains(out, "REPO_ID") || !strings.Contains(out, "KIND") || !strings.Contains(out, "BRANCH") {
		t.Fatalf("missing expected headers in:\n%s", out)
	}
	idIdx := strings.Index(out, "REPO_ID")
	kindIdx := strings.Index(out, "KIND")
	branchIdx := strings.Index(out, "BRANCH")
	if idIdx >= kindIdx || kindIdx >= branchIdx {
		t.Errorf("KIND must sit between REPO_ID and BRANCH; got header order:\n%s", out)
	}
	if !strings.Contains(out, "tracked") {
		t.Errorf("missing 'tracked' value: %s", out)
	}
	if !strings.Contains(out, "ephemeral") {
		t.Errorf("missing 'ephemeral' value: %s", out)
	}
}

func TestPrintRepoTable_BlankKindDefaultsToTracked(t *testing.T) {
	var buf bytes.Buffer
	PrintRepoTable(&buf, []RepoView{
		{RepoID: "aaaaaaaaaaaaaaaaaa", RootPath: "/tmp/a", ActiveBranch: "main", LastPromotedSHA: "sha"},
	})
	if !strings.Contains(buf.String(), "tracked") {
		t.Errorf("empty Kind should render as 'tracked', got:\n%s", buf.String())
	}
}

func TestPrintRepoTable_EmptyMessageUnchanged(t *testing.T) {
	var buf bytes.Buffer
	PrintRepoTable(&buf, nil)
	want := "no repositories registered - run: veska repo add <path>"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("missing empty-message %q; got:\n%s", want, buf.String())
	}
}
