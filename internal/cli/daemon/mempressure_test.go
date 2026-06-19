// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// fakeAvail returns a func() (uint64, bool) reporting a fixed available-byte
// count, simulating the injected memory reader.
func fakeAvail(bytesAvail uint64, ok bool) func() (uint64, bool) {
	return func() (uint64, bool) { return bytesAvail, ok }
}

// TestUnderMemoryPressure_BelowFloor pins: the predicate reports pressure when
// available RAM is below the runtime pressure floor, and no pressure above it.
func TestUnderMemoryPressure_BelowFloor(t *testing.T) {
	if !underMemoryPressure(fakeAvail(pressureFloorBytes-1, true)) {
		t.Fatalf("expected pressure when avail < pressureFloorBytes")
	}
	if underMemoryPressure(fakeAvail(pressureFloorBytes+1, true)) {
		t.Fatalf("expected no pressure when avail > pressureFloorBytes")
	}
}

// TestUnderMemoryPressure_UnsupportedIsNoOp pins: when the reader is
// unsupported (ok=false) or nil, the predicate degrades to a no-op (false), so
// non-linux platforms never pause.
func TestUnderMemoryPressure_UnsupportedIsNoOp(t *testing.T) {
	if underMemoryPressure(fakeAvail(0, false)) {
		t.Fatalf("unsupported reader must not report pressure")
	}
	if underMemoryPressure(nil) {
		t.Fatalf("nil reader must not report pressure")
	}
}

// TestIngestionBusyPredicate_MemoryPressure pins that the shared pauser flips to
// true under memory pressure even with no scan/resync running, and stays true
// during a scan regardless of memory.
func TestIngestionBusyPredicate_MemoryPressure(t *testing.T) {
	b := &daemonBuilder{}
	b.buildIngestionBusy(fakeAvail(pressureFloorBytes-1, true))
	if !b.ingestionBusy() {
		t.Fatalf("expected ingestionBusy=true under memory pressure")
	}

	// Above the floor with no scan/resync: not busy.
	b2 := &daemonBuilder{}
	b2.buildIngestionBusy(fakeAvail(pressureFloorBytes*4, true))
	if b2.ingestionBusy() {
		t.Fatalf("expected ingestionBusy=false with ample memory and no scan")
	}

	// During a scan, busy regardless of ample memory.
	b2.scanTracker.Start("repo-1")
	if !b2.ingestionBusy() {
		t.Fatalf("expected ingestionBusy=true while a scan runs")
	}
}

// TestMaybeWarnLowMemory pins the startup advisory: it fires for the memory
// backend below the advisory floor, and stays silent above the floor, for a
// non-memory backend, or when the reader is unsupported.
func TestMaybeWarnLowMemory(t *testing.T) {
	const wantPhrase = "low available memory"

	cases := []struct {
		name    string
		backend vector.BackendKind
		avail   func() (uint64, bool)
		want    bool
	}{
		{"memvec below floor warns", vector.BackendMemory, fakeAvail(advisoryFloorBytes-1, true), true},
		{"memvec above floor silent", vector.BackendMemory, fakeAvail(advisoryFloorBytes+1, true), false},
		{"usearch below floor silent", vector.BackendUsearch, fakeAvail(advisoryFloorBytes-1, true), false},
		{"unsupported reader silent", vector.BackendMemory, fakeAvail(0, false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			maybeWarnLowMemory(tc.backend, tc.avail, logger)
			got := strings.Contains(buf.String(), wantPhrase)
			if got != tc.want {
				t.Fatalf("warn fired=%v want=%v; log=%q", got, tc.want, buf.String())
			}
		})
	}
}
