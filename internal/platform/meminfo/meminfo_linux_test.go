// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package meminfo_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/meminfo"
)

// TestAvailable_ReportsPositive pins that the linux reader returns a non-zero
// byte count and no error on a running system. The Unit multiplication must be
// applied, so the value lands in byte-scale (many MiB), not raw page counts.
func TestAvailable_ReportsPositive(t *testing.T) {
	got, err := meminfo.Available()
	if err != nil {
		t.Fatalf("Available() error: %v", err)
	}
	if got == 0 {
		t.Fatalf("Available() = 0, want > 0 on a running system")
	}
	// Sanity: any real machine running tests has at least a few MiB free.
	const oneMiB = 1 << 20
	if got < oneMiB {
		t.Fatalf("Available() = %d bytes, implausibly low (Unit not applied?)", got)
	}
}
