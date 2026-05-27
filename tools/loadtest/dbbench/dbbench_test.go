//go:build eval

package dbbench

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDBBench runs the full driver comparison and emits RESULTS.md.
// Override via env:
//
//	DBBENCH_DRIVERS  comma-list (default: all registered)
//	DBBENCH_NODES    seed node count (default 10000)
//	DBBENCH_DB       path to an existing veska.db; if set, the harness
//	                 reuses it (and DOES NOT seed). Schema-compat is the
//	                 caller's responsibility — use a fresh mattn-built db.
//	DBBENCH_OUT      output path for RESULTS.md (default: alongside test file)
//	DBBENCH_QUICK    if set, slashes iter counts ~10× for a smoke run.
func TestDBBench(t *testing.T) {
	ctx := context.Background()

	drivers := Drivers()
	if env := os.Getenv("DBBENCH_DRIVERS"); env != "" {
		drivers = strings.Split(env, ",")
	}
	if len(drivers) == 0 {
		t.Fatal("no drivers registered; build with -tags=eval (and cgo for mattn)")
	}

	seed := DefaultSeedConfig()
	if v := os.Getenv("DBBENCH_NODES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("invalid DBBENCH_NODES=%q", v)
		}
		seed.Nodes = n
	}

	cfg := DefaultRunConfig()
	if os.Getenv("DBBENCH_QUICK") != "" {
		cfg.GraphIters = 200
		cfg.FTSIters = 100
		cfg.QueueIters = 100
		cfg.PromotionTx = 50
		cfg.BulkIters = 5
		cfg.RehydrateRuns = 2
	}

	stmts, err := SchemaStatements()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	snapshot := os.Getenv("DBBENCH_DB")
	all := make([]WorkloadResult, 0, len(drivers)*6)

	for _, name := range drivers {
		t.Run(name, func(t *testing.T) {
			b, err := NewBench(name)
			if err != nil {
				t.Fatalf("NewBench: %v", err)
			}
			defer b.Close()

			dbPath := snapshot
			if dbPath == "" {
				dbPath = filepath.Join(t.TempDir(), "bench.db")
			} else {
				// Copy the snapshot so write workloads don't mutate it across drivers.
				dbPath = copySnapshot(t, snapshot)
			}

			if err := b.Open(ctx, dbPath); err != nil {
				t.Fatalf("Open: %v", err)
			}

			if snapshot == "" {
				if err := b.ApplySchema(ctx, stmts); err != nil {
					t.Fatalf("ApplySchema: %v", err)
				}
				if err := b.Seed(ctx, seed); err != nil {
					t.Fatalf("Seed: %v", err)
				}
			}

			res, err := Run(ctx, b, cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			all = append(all, res...)
			for _, r := range res {
				t.Logf("%-10s %-15s p50=%.3fms p95=%.3fms ops/s=%.1f",
					r.Driver, r.Workload, r.P50Ms, r.P95Ms, r.OpsPerS)
			}
		})
	}

	if t.Failed() {
		return
	}
	out := os.Getenv("DBBENCH_OUT")
	if out == "" {
		out = "RESULTS.md"
	}
	if err := WriteReport(out, drivers, all, cfg, seed); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	t.Logf("wrote %s", out)
}

func copySnapshot(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "bench.db")
	in, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if err := os.WriteFile(dst, in, 0o644); err != nil {
		t.Fatalf("write snapshot copy: %v", err)
	}
	return dst
}
