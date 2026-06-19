// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

// Package meminfo is a zero-dependency platform leaf that reports available
// system RAM. It exists so daemon lanes can back off under memory pressure
// instead of pushing the process to OOM (solov2-btpj). Mechanism only - the
// floor/threshold policy lives with its consumer (the daemon), mirroring the
// doctor freebytes leaf which reports disk free bytes without baking in policy.
package meminfo

import "syscall"

// Available returns the number of bytes of RAM the kernel reports as free,
// using syscall.Sysinfo (stdlib, no CGo). The call stack-allocates its struct
// per invocation, so it is stateless and safe to call concurrently from the
// queue/embedder poll loops without any shared mutable state.
//
// Note: Sysinfo.Freeram counts only genuinely free pages, not reclaimable
// page-cache, so it under-reports the kernel's true MemAvailable. For a floor
// check that bias is safe: it errs toward warning/pausing slightly early rather
// than letting the daemon run into OOM.
func Available() (uint64, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, err
	}
	// Sysinfo reports memory in units of info.Unit bytes; multiplying by Unit
	// is required (forgetting it is the classic Sysinfo bug).
	return info.Freeram * uint64(info.Unit), nil
}
