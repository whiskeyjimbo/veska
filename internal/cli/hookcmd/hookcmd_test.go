// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package hookcmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestIsGitSpecialState_Clean verifies that a clean git root (no special files) returns false.
func TestIsGitSpecialState_Clean(t *testing.T) {
	dir := t.TempDir()
	if IsGitSpecialState(dir) {
		t.Error("expected false for clean git root, got true")
	}
}

// TestIsGitSpecialState_MergeHead detects.git/MERGE_HEAD.
func TestIsGitSpecialState_MergeHead(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsGitSpecialState(dir) {
		t.Error("expected true for MERGE_HEAD, got false")
	}
}

// TestIsGitSpecialState_CherryPickHead detects.git/CHERRY_PICK_HEAD.
func TestIsGitSpecialState_CherryPickHead(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "CHERRY_PICK_HEAD"), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsGitSpecialState(dir) {
		t.Error("expected true for CHERRY_PICK_HEAD, got false")
	}
}

// TestIsGitSpecialState_BisectLog detects.git/BISECT_LOG.
func TestIsGitSpecialState_BisectLog(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "BISECT_LOG"), []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsGitSpecialState(dir) {
		t.Error("expected true for BISECT_LOG, got false")
	}
}

// TestIsGitSpecialState_RebaseMergeDir detects.git/rebase-merge/ directory.
func TestIsGitSpecialState_RebaseMergeDir(t *testing.T) {
	dir := t.TempDir()
	rebaseDir := filepath.Join(dir, ".git", "rebase-merge")
	if err := os.MkdirAll(rebaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsGitSpecialState(dir) {
		t.Error("expected true for rebase-merge dir, got false")
	}
}

// TestIsGitSpecialState_RebaseApplyDir detects.git/rebase-apply/ directory.
func TestIsGitSpecialState_RebaseApplyDir(t *testing.T) {
	dir := t.TempDir()
	rebaseDir := filepath.Join(dir, ".git", "rebase-apply")
	if err := os.MkdirAll(rebaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsGitSpecialState(dir) {
		t.Error("expected true for rebase-apply dir, got false")
	}
}

// TestSendSeal_NoSocket: a missing socket now returns a non-nil dial error so
// the caller (RunPostCommit) can fall back to the next candidate path
// The outer hook still ignores this error and exits 0.
func TestSendSeal_NoSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "nonexistent.sock")
	err := SendSeal(sockPath)
	if err == nil {
		t.Errorf("expected non-nil dial error for missing socket, got nil")
	}
}

// TestSendSeal_DaemonRespondsOK verifies a successful round-trip with a fake daemon.
func TestSendSeal_DaemonRespondsOK(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		conn.Read(buf) //nolint:errcheck
		conn.Write([]byte(`{"ok":true}` + "\n"))
	}()

	if err := SendSeal(sockPath); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	<-done
}

// TestSendSeal_DaemonTimeout verifies that a slow/silent daemon is treated as success.
func TestSendSeal_DaemonTimeout(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept but never respond - simulates a hung daemon.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		conn.Read(buf) //nolint:errcheck
		// deliberately do not write a response
	}()

	if err := SendSeal(sockPath); err != nil {
		t.Errorf("expected nil on timeout, got %v", err)
	}
}
