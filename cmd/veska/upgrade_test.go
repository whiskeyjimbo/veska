// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeCmdName(t *testing.T) {
	cmd := upgradeCmd(nil)
	if cmd.Use != "upgrade <path>" {
		t.Errorf("expected Use=%q, got %q", "upgrade <path>", cmd.Use)
	}
}

func TestUpgradeSwapsFile(t *testing.T) {
	// Create a temporary "new binary" with executable bit set.
	newBin := filepath.Join(t.TempDir(), "newbinary")
	newContent := []byte("#!/bin/sh\necho new\n")
	if err := os.WriteFile(newBin, newContent, 0755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}

	// Create a temporary "old binary" that will be replaced.
	oldBin := filepath.Join(t.TempDir(), "oldbinary")
	oldContent := []byte("#!/bin/sh\necho old\n")
	if err := os.WriteFile(oldBin, oldContent, 0755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}

	cmd := upgradeCmd(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--target", oldBin, newBin})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(oldBin)
	if err != nil {
		t.Fatalf("read target after swap: %v", err)
	}
	if !bytes.Equal(got, newContent) {
		t.Errorf("target content after swap = %q, want %q", got, newContent)
	}

	// Verify output mentions old and new paths.
	out := buf.String()
	if !strings.Contains(out, oldBin) {
		t.Errorf("output %q does not mention target path %q", out, oldBin)
	}
	if !strings.Contains(out, newBin) {
		t.Errorf("output %q does not mention source path %q", out, newBin)
	}
}

func TestUpgradeRejectsNonExistent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := upgradeCmd(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--target", target, "/nonexistent/path/binary"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent source path, got nil")
	}
}

func TestUpgradeRejectsNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute-bit check not applicable on Windows")
	}

	newBin := filepath.Join(t.TempDir(), "nonexec")
	if err := os.WriteFile(newBin, []byte("#!/bin/sh\necho hi\n"), 0644); err != nil {
		t.Fatalf("write non-executable file: %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := upgradeCmd(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--target", target, newBin})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-executable source, got nil")
	}
	if !strings.Contains(err.Error(), "executable") {
		t.Errorf("error %q does not mention 'executable'", err.Error())
	}
}

func TestUpgradeWithRestart(t *testing.T) {
	newBin := filepath.Join(t.TempDir(), "newbinary")
	if err := os.WriteFile(newBin, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "oldbinary")
	if err := os.WriteFile(target, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	stub := &stubServiceManager{}
	cmd := upgradeCmd(stub)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--target", target, "--restart", newBin})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stub.lastCall != "restart" {
		t.Errorf("expected stub.lastCall=restart, got %q", stub.lastCall)
	}
}
