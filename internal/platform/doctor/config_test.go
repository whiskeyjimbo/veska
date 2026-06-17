package doctor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

func TestCheckConfigExists(t *testing.T) {
	dir := t.TempDir()

	// Write a fake veska.db to the temp dir.
	dbPath := filepath.Join(dir, "veska.db")
	if err := os.WriteFile(dbPath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	report, err := doctor.CheckConfig(dir)
	if err != nil {
		t.Fatalf("CheckConfig: unexpected error: %v", err)
	}
	if !report.DBExists {
		t.Errorf("DBExists: got false, want true")
	}
	if report.VeskaHome != dir {
		t.Errorf("VeskaHome: got %q, want %q", report.VeskaHome, dir)
	}
	if report.DBPath != dbPath {
		t.Errorf("DBPath: got %q, want %q", report.DBPath, dbPath)
	}
}

func TestCheckConfigMissing(t *testing.T) {
	dir := t.TempDir()

	// No veska.db written - empty dir.
	report, err := doctor.CheckConfig(dir)
	if err != nil {
		t.Fatalf("CheckConfig: unexpected error: %v", err)
	}
	if report.DBExists {
		t.Errorf("DBExists: got true, want false")
	}
	if report.VeskaHome != dir {
		t.Errorf("VeskaHome: got %q, want %q", report.VeskaHome, dir)
	}
}

func TestCheckConfigVeskaHomeSet(t *testing.T) {
	dir := t.TempDir()

	// Set VESKA_HOME to dir so VeskaHomeSet should be true.
	t.Setenv("VESKA_HOME", dir)

	report, err := doctor.CheckConfig(dir)
	if err != nil {
		t.Fatalf("CheckConfig: unexpected error: %v", err)
	}
	if !report.VeskaHomeSet {
		t.Errorf("VeskaHomeSet: got false, want true when VESKA_HOME is set")
	}
}

func TestCheckConfigVeskaHomeNotSet(t *testing.T) {
	dir := t.TempDir()

	// Ensure VESKA_HOME is unset.
	t.Setenv("VESKA_HOME", "")

	report, err := doctor.CheckConfig(dir)
	if err != nil {
		t.Fatalf("CheckConfig: unexpected error: %v", err)
	}
	if report.VeskaHomeSet {
		t.Errorf("VeskaHomeSet: got true, want false when VESKA_HOME is empty")
	}
}
