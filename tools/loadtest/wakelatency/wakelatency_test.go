// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Package wakelatency's eval test: drives the production
// git.WakeReconciler mtime/size/prefix sweep against a synthetic on-disk
// tree and asserts the / wake-reconcile
// latency NFR:
//
//	typical repo: sweep p95 < 500ms over N >= 20 InjectWake iterations.
//	>50k files: a single worst-case sweep < 5s.
//
// The sweep cost the NFR targets is the no-change full walk: stat +
// 64-byte prefix read + last-seen map compare on every tracked file, with
// the handler never firing. After Seed records the baseline, a no-change
// InjectWake still walks every file but invokes no handler.
// Only InjectWake is timed; synthetic-tree generation and Seed are
// setup. The single-repo NFR is one goroutine (WithWakeConcurrency is a
// no-op for a single registered tree), so no -race is needed.
// Build-tag-gated so plain CI runs (`go test./.`) skip this harness.
// The make target is `make eval-wake-latency`.
package wakelatency

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

const (
	defaultTypicalFiles = 5000
	defaultLargeFiles   = 50000

	filesPerDir = 100

	iterations = 25 // N >= 20 InjectWake iterations for the typical p95.

	gateP95MS   = 500.0  // typical repo p95 threshold (ms).
	gateLargeMS = 5000.0 // >50k worst-case threshold (ms).

	wakeTick      = time.Second
	wakeThreshold = time.Second
)

// TestWakeLatency is the wake-reconcile sweep latency gate. Gate (a):
// typical-tree p95 < 500ms. Gate (b): 50k-tree single sweep < 5s. Either
// breach fails the NFR.
func TestWakeLatency(t *testing.T) {
	typicalFiles := envInt("WAKE_FILES", defaultTypicalFiles)
	largeFiles := envInt("WAKE_FILES_LARGE", defaultLargeFiles)

	// Gate (a): p95 over N iterations on the typical tree.
	typical := measureSweeps(t, "typical", typicalFiles, iterations)
	slices.Sort(typical)
	p95 := percentile(typical, 95)
	typMin, typMax := typical[0], typical[len(typical)-1]

	// Gate (b): single worst-case sweep on the >50k tree.
	large := measureSweeps(t, "large", largeFiles, 1)
	worst := large[0]

	p95MS := msOf(p95)
	worstMS := msOf(worst)
	gateAMet := p95MS < gateP95MS
	gateBMet := worstMS < gateLargeMS

	res := Result{
		TypicalFiles: typicalFiles,
		LargeFiles:   largeFiles,
		Iterations:   iterations,
		TypicalP95MS: p95MS,
		TypicalMinMS: msOf(typMin),
		TypicalMaxMS: msOf(typMax),
		LargeWorstMS: worstMS,
		GateP95MS:    gateP95MS,
		GateLargeMS:  gateLargeMS,
		ExitGateMet:  gateAMet && gateBMet,
		Backend:      "git.WakeReconciler",
		Timestamp:    time.Now().UTC(),
	}

	gate := "PASS"
	if !res.ExitGateMet {
		gate = "FAIL"
	}
	fmt.Printf("WAKE typical_files=%d p95_ms=%.2f (min=%.2f max=%.2f) large_files=%d worst_ms=%.2f gate=%s\n",
		res.TypicalFiles, res.TypicalP95MS, res.TypicalMinMS, res.TypicalMaxMS,
		res.LargeFiles, res.LargeWorstMS, gate)

	if err := WriteJSON("wake_latency_results.json", res); err != nil {
		t.Logf("WriteJSON: %v (continuing)", err)
	}

	if !gateAMet {
		t.Fatalf("NFR gate (a) FAILED: typical-repo p95=%.2fms >= %.0fms (%d files, %d iters)",
			p95MS, gateP95MS, typicalFiles, iterations)
	}
	if !gateBMet {
		t.Fatalf("NFR gate (b) FAILED: 50k worst-case sweep=%.2fms >= %.0fms (%d files)",
			worstMS, gateLargeMS, largeFiles)
	}
}

// measureSweeps generates a synthetic tree of n files, seeds the
// reconciler baseline, then times `iters` no-change InjectWake sweeps.
// It asserts the handler never fired (a nonzero count would mean the timed
// sweep includes handler work and the number isn't the pure walk).
func measureSweeps(t *testing.T, tag string, n, iters int) []time.Duration {
	t.Helper()
	root := t.TempDir()
	generateTree(t, root, n)

	var fired atomic.Int64
	handler := func(ctx context.Context, repoID, path string) { fired.Add(1) }

	r := git.NewWakeReconciler(wakeTick, wakeThreshold, handler)
	r.AddDir(tag, root) // single tree -> recurses into all subdirs.
	// Seed the baseline via an initial (untimed) no-change sweep: with no
	// BaselineStore wired, the reconciler's standalone baseline records each
	// file on first sighting and fires nothing (Seed was retired in
	// ). The timed sweeps below are no-change full walks.
	r.InjectWake()

	durs := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		start := time.Now()
		r.InjectWake()
		durs = append(durs, time.Since(start))
	}

	if got := fired.Load(); got != 0 {
		t.Fatalf("[%s] handler fired %d times on a no-change sweep: timed walk is not pure", tag, got)
	}
	return durs
}

// generateTree writes n deterministic small source-like files into root,
// spread filesPerDir per subdir. File sizes are ~1-4KB so the reconciler's
// 64-byte prefix read is representative of a real source tree.
func generateTree(t *testing.T, root string, n int) {
	t.Helper()
	var dir string
	for i := 0; i < n; i++ {
		if i%filesPerDir == 0 {
			dir = filepath.Join(root, fmt.Sprintf("pkg%05d", i/filesPerDir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		path := filepath.Join(dir, fmt.Sprintf("f%05d.go", i))
		if err := os.WriteFile(path, fileBody(i), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// fileBody returns deterministic ~1-4KB content for file i.
func fileBody(i int) []byte {
	size := 1024 + (i%4)*1024 // 1,2,3,4 KB cycling deterministically.
	buf := make([]byte, 0, size)
	header := fmt.Sprintf("// file %d\npackage gen\n\n", i)
	buf = append(buf, header...)
	line := []byte("var _ = 0xDEADBEEF // synthetic source filler line\n")
	for len(buf) < size {
		buf = append(buf, line...)
	}
	return buf[:size]
}

// percentile returns the p-th percentile of a sorted slice (nearest-rank).
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) * p) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func msOf(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
