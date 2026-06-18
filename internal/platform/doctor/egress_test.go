// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

func TestCheckEgressReachable(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	report, err := doctor.CheckEgress([]string{sockPath})
	if err != nil {
		t.Fatalf("CheckEgress: unexpected error: %v", err)
	}
	if len(report.Sockets) != 1 {
		t.Fatalf("Sockets: got %d, want 1", len(report.Sockets))
	}
	if report.Sockets[0].Status != "reachable" {
		t.Errorf("Status: got %q, want %q", report.Sockets[0].Status, "reachable")
	}
	if report.Sockets[0].Path != sockPath {
		t.Errorf("Path: got %q, want %q", report.Sockets[0].Path, sockPath)
	}
}

func TestCheckEgressMissing(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "nonexistent.sock")

	// Ensure the socket does not exist.
	_ = os.Remove(sockPath)

	report, err := doctor.CheckEgress([]string{sockPath})
	if err != nil {
		t.Fatalf("CheckEgress: unexpected error: %v", err)
	}
	if len(report.Sockets) != 1 {
		t.Fatalf("Sockets: got %d, want 1", len(report.Sockets))
	}
	if report.Sockets[0].Status != "missing" {
		t.Errorf("Status: got %q, want %q", report.Sockets[0].Status, "missing")
	}
}
