// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResetCrashLoopBothPresent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "broken"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crash_count"), []byte("3"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := ResetCrashLoop(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.BrokenMarkerCleared {
		t.Error("expected BrokenMarkerCleared=true")
	}
	if !report.CrashCountCleared {
		t.Error("expected CrashCountCleared=true")
	}
	if report.CrashCountWas != 3 {
		t.Errorf("expected CrashCountWas=3, got %d", report.CrashCountWas)
	}
	if _, err := os.Stat(filepath.Join(dir, "broken")); !os.IsNotExist(err) {
		t.Error("broken marker should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "crash_count")); !os.IsNotExist(err) {
		t.Error("crash_count should be gone")
	}
}

func TestResetCrashLoopNothingPresent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	report, err := ResetCrashLoop(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.BrokenMarkerCleared {
		t.Error("expected BrokenMarkerCleared=false")
	}
	if report.CrashCountCleared {
		t.Error("expected CrashCountCleared=false")
	}
	if report.CrashCountWas != 0 {
		t.Errorf("expected CrashCountWas=0, got %d", report.CrashCountWas)
	}
}

func TestResetCrashLoopOnlyBroken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "broken"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := ResetCrashLoop(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !report.BrokenMarkerCleared {
		t.Error("expected BrokenMarkerCleared=true")
	}
	if report.CrashCountCleared {
		t.Error("expected CrashCountCleared=false")
	}
	if report.CrashCountWas != 0 {
		t.Errorf("expected CrashCountWas=0, got %d", report.CrashCountWas)
	}
}

func TestResetCrashLoopOnlyCrashCount(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "crash_count"), []byte("7"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := ResetCrashLoop(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.BrokenMarkerCleared {
		t.Error("expected BrokenMarkerCleared=false")
	}
	if !report.CrashCountCleared {
		t.Error("expected CrashCountCleared=true")
	}
	if report.CrashCountWas != 7 {
		t.Errorf("expected CrashCountWas=7, got %d", report.CrashCountWas)
	}
}
