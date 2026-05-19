package crashloop_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/crashloop"
)

func TestCheckNoBroken(t *testing.T) {
	dir := t.TempDir()
	if err := crashloop.Check(dir); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestCheckBroken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	err := crashloop.Check(dir)
	if err == nil {
		t.Fatal("expected ErrBroken, got nil")
		return
	}
	if err != crashloop.ErrBroken {
		t.Fatalf("expected ErrBroken sentinel, got %v", err)
	}
}

func TestRecordUnderThreshold(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 4; i++ {
		tripped, err := crashloop.Record(dir)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if tripped {
			t.Fatalf("call %d: expected tripped=false", i)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "crash_count"))
	if err != nil {
		t.Fatalf("reading crash_count: %v", err)
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatalf("parsing crash_count: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected crash_count=4, got %d", n)
	}
}

func TestRecordTripsAtFive(t *testing.T) {
	dir := t.TempDir()
	var tripped bool
	var err error
	for i := 1; i <= 5; i++ {
		tripped, err = crashloop.Record(dir)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if i < 5 && tripped {
			t.Fatalf("call %d: expected tripped=false before threshold", i)
		}
	}
	if !tripped {
		t.Fatal("expected tripped=true on 5th call")
	}
	if _, err := os.Stat(filepath.Join(dir, "broken")); os.IsNotExist(err) {
		t.Fatal("expected broken file to exist after trip")
	}
}

func TestRecordResetsAfterWindow(t *testing.T) {
	dir := t.TempDir()

	// Write a window start 11 minutes in the past.
	oldTime := time.Now().Add(-11 * time.Minute)
	windowPath := filepath.Join(dir, "crash_window_start")
	if err := os.WriteFile(windowPath, []byte(strconv.FormatInt(oldTime.Unix(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set count to 4 so it would trip if not reset.
	if err := os.WriteFile(filepath.Join(dir, "crash_count"), []byte("4"), 0o600); err != nil {
		t.Fatal(err)
	}

	tripped, err := crashloop.Record(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tripped {
		t.Fatal("expected tripped=false after window reset")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "crash_count"))
	n, _ := strconv.Atoi(string(raw))
	if n != 1 {
		t.Fatalf("expected crash_count=1 after reset, got %d", n)
	}
}

func TestRecordIdempotentAfterTrip(t *testing.T) {
	dir := t.TempDir()

	// Trip it.
	for i := range 5 {
		if _, err := crashloop.Record(dir); err != nil {
			t.Fatalf("setup call %d failed: %v", i, err)
		}
	}

	// Call again after trip.
	tripped, err := crashloop.Record(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tripped {
		t.Fatal("expected tripped=true when broken file already exists")
	}
}
