package doctor_test

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/doctor"
)

func TestCheckServiceHealthy(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "daemon.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	report, err := doctor.CheckService(dir)
	if err != nil {
		t.Fatalf("CheckService: unexpected error: %v", err)
	}
	if !report.DaemonRunning {
		t.Errorf("DaemonRunning: got false, want true")
	}
	if report.BrokenMarkerPresent {
		t.Errorf("BrokenMarkerPresent: got true, want false")
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}
}

func TestCheckServiceDegraded(t *testing.T) {
	dir := t.TempDir()
	// No daemon socket created.

	report, err := doctor.CheckService(dir)
	if err != nil {
		t.Fatalf("CheckService: unexpected error: %v", err)
	}
	if report.DaemonRunning {
		t.Errorf("DaemonRunning: got true, want false")
	}
	if report.BrokenMarkerPresent {
		t.Errorf("BrokenMarkerPresent: got true, want false")
	}
	if report.Status != "degraded" {
		t.Errorf("Status: got %q, want %q", report.Status, "degraded")
	}
}

func TestCheckServiceBroken(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "broken")
	if err := os.WriteFile(markerPath, []byte("crash-loop"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := doctor.CheckService(dir)
	if err != nil {
		t.Fatalf("CheckService: unexpected error: %v", err)
	}
	if !report.BrokenMarkerPresent {
		t.Errorf("BrokenMarkerPresent: got false, want true")
	}
	if report.BrokenMarkerPath != markerPath {
		t.Errorf("BrokenMarkerPath: got %q, want %q", report.BrokenMarkerPath, markerPath)
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q", report.Status, "broken")
	}
}

func TestCheckServiceBrokenTakesPriority(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "daemon.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	markerPath := filepath.Join(dir, "broken")
	if err := os.WriteFile(markerPath, []byte("crash-loop"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := doctor.CheckService(dir)
	if err != nil {
		t.Fatalf("CheckService: unexpected error: %v", err)
	}
	if !report.DaemonRunning {
		t.Errorf("DaemonRunning: got false, want true")
	}
	if !report.BrokenMarkerPresent {
		t.Errorf("BrokenMarkerPresent: got false, want true")
	}
	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q", report.Status, "broken")
	}
}
