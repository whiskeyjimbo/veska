// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
)

// startCLIProfiling enables ad-hoc profiling of a single CLI run when the
// VESKA_CPUPROFILE / VESKA_MEMPROFILE env vars name output files. It is the
// CLI counterpart to the daemon's HTTP pprof endpoint (VESKA_PPROF): a
// short-lived CLI has no server to scrape, so it writes profile files directly.
//
// It returns a stop func that the caller MUST run before os.Exit - main()
// terminates several paths with os.Exit, which skips deferred funcs, so the
// CPU profile would be empty/truncated if relied on a defer. main() therefore
// computes an exit code, calls stop(), then exits.
func startCLIProfiling() func() {
	var stops []func()

	if path := os.Getenv("VESKA_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "veska: cpuprofile create %s: %v\n", path, err)
		} else if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "veska: cpuprofile start: %v\n", err)
			_ = f.Close()
		} else {
			stops = append(stops, func() {
				pprof.StopCPUProfile()
				_ = f.Close()
			})
		}
	}

	if path := os.Getenv("VESKA_MEMPROFILE"); path != "" {
		// Heap profiles are a snapshot taken at stop time, so the write is
		// deferred into the stop func and run after the command completes.
		stops = append(stops, func() {
			f, err := os.Create(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "veska: memprofile create %s: %v\n", path, err)
				return
			}
			defer f.Close()
			runtime.GC() // materialize up-to-date live-heap stats
			if err := pprof.WriteHeapProfile(f); err != nil {
				fmt.Fprintf(os.Stderr, "veska: memprofile write: %v\n", err)
			}
		})
	}

	return func() {
		for _, stop := range stops {
			stop()
		}
	}
}
