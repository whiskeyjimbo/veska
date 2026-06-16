package doctor_test

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// writeTarGz writes a minimal valid.tar.gz archive to path.
func writeTarGz(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("writeTarGz: create: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	content := []byte("veska backup test content")
	hdr := &tar.Header{
		Name:    "test.txt",
		Mode:    0o644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("writeTarGz: write header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("writeTarGz: write content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("writeTarGz: close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("writeTarGz: close gzip: %v", err)
	}
}

// writeInvalidGz writes a file with a.tar.gz extension but invalid gzip content.
func writeInvalidGz(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("this is not gzip data"), 0o644); err != nil {
		t.Fatalf("writeInvalidGz: %v", err)
	}
}

func TestCheckBackupHealthy(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "backup-2026-01-01.tar.gz")
	writeTarGz(t, archivePath)

	report, err := doctor.CheckBackup(dir)
	if err != nil {
		t.Fatalf("CheckBackup: unexpected error: %v", err)
	}

	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}
	if report.FileCount != 1 {
		t.Errorf("FileCount: got %d, want 1", report.FileCount)
	}
	if report.LatestFile != archivePath {
		t.Errorf("LatestFile: got %q, want %q", report.LatestFile, archivePath)
	}
	if report.VerifyError != "" {
		t.Errorf("VerifyError: got %q, want empty", report.VerifyError)
	}
	// Age should be very small (just created).
	if report.AgeHours < 0 {
		t.Errorf("AgeHours: got %f, should be >= 0", report.AgeHours)
	}
	if report.AgeHours > 1 {
		t.Errorf("AgeHours: got %f, want < 1 (just created)", report.AgeHours)
	}
	if report.BackupDir != dir {
		t.Errorf("BackupDir: got %q, want %q", report.BackupDir, dir)
	}
}

func TestCheckBackupNoneFound(t *testing.T) {
	dir := t.TempDir()

	report, err := doctor.CheckBackup(dir)
	if err != nil {
		t.Fatalf("CheckBackup: unexpected error: %v", err)
	}

	if report.Status != "degraded" {
		t.Errorf("Status: got %q, want %q", report.Status, "degraded")
	}
	if report.FileCount != 0 {
		t.Errorf("FileCount: got %d, want 0", report.FileCount)
	}
	if report.LatestFile != "" {
		t.Errorf("LatestFile: got %q, want empty", report.LatestFile)
	}
}

func TestCheckBackupBroken(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "backup-bad.tar.gz")
	writeInvalidGz(t, archivePath)

	report, err := doctor.CheckBackup(dir)
	if err != nil {
		t.Fatalf("CheckBackup: unexpected error: %v", err)
	}

	if report.Status != "broken" {
		t.Errorf("Status: got %q, want %q", report.Status, "broken")
	}
	if report.VerifyError == "" {
		t.Error("VerifyError: got empty, want non-empty gzip error")
	}
	if report.LatestFile != archivePath {
		t.Errorf("LatestFile: got %q, want %q", report.LatestFile, archivePath)
	}
}

func TestCheckBackupPicksMostRecent(t *testing.T) {
	dir := t.TempDir()

	olderPath := filepath.Join(dir, "backup-older.tar.gz")
	newerPath := filepath.Join(dir, "backup-newer.tar.gz")

	writeTarGz(t, olderPath)
	writeTarGz(t, newerPath)

	// Set mtime on olderPath to 2 hours ago so newerPath (default now) is more recent.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(olderPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	report, err := doctor.CheckBackup(dir)
	if err != nil {
		t.Fatalf("CheckBackup: unexpected error: %v", err)
	}

	if report.LatestFile != newerPath {
		t.Errorf("LatestFile: got %q, want %q (newer)", report.LatestFile, newerPath)
	}
	if report.FileCount != 2 {
		t.Errorf("FileCount: got %d, want 2", report.FileCount)
	}
	if report.Status != "healthy" {
		t.Errorf("Status: got %q, want %q", report.Status, "healthy")
	}
}
