// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Batch-size + WAL sweep for the cold-scan embed drain. Once the model2vec
// embed compute fans across cores (the GOMAXPROCS-sized governor), the serial
// SQLite write drain is the next candidate ceiling. This drives the REAL
// embedder.Worker (real model2vec provider + GOMAXPROCS governor + real
// migrated SQLite, no Ollama) over a finite pending queue to empty - the
// cold-scan shape - and reports drain wall-time and WAL pressure per arm.
//
// Arms: NORMAL@{32,128,256} isolates the batch-size amortization lever;
// FULL@256 vs NORMAL@256 bounds WAL fsync's share of the drain (if it barely
// moves wall-time, fsync is not the ceiling and batch size is about per-tx /
// prepare amortization, not durability cost).
//
// Run: `make eval-embed-batchsize` (or `go test -tags eval -run
// TestEmbedBatchSizeWALSweep -timeout 20m ./tools/loadtest/embedder`).
// Tune with EMBED_BATCH_N (default 50000). Skips when model2vec is not
// installed under VESKA_HOME.
package embedder

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector/memvec"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

type armResult struct {
	batchSize  int
	syncMode   string
	syncPragma int
	n          int
	elapsed    time.Duration
	ratePerSec float64
	walBytes   int64
	logFrames  int
	ckptFrames int
}

func TestEmbedBatchSizeWALSweep(t *testing.T) {
	dir := model2vecDir()
	if dir == "" {
		t.Skip("model2vec not installed under VESKA_HOME - run 'veska install model2vec'")
	}
	n := envInt("EMBED_BATCH_N", 50000)
	gomaxprocs := runtime.GOMAXPROCS(0)

	arms := []struct {
		batchSize int
		syncMode  string
	}{
		{32, "NORMAL"},
		{128, "NORMAL"},
		{256, "NORMAL"},
		{256, "FULL"},
	}

	results := make([]armResult, 0, len(arms))
	for _, a := range arms {
		results = append(results, runDrainArm(t, dir, n, a.batchSize, a.syncMode, gomaxprocs))
	}

	t.Logf("=== embed batch-size + WAL sweep (n=%d, GOMAXPROCS=%d) ===", n, gomaxprocs)
	t.Logf("%-6s %-7s %-5s %12s %12s %12s %10s %10s",
		"batch", "sync", "pragm", "drain", "rate/s", "wal_bytes", "log_frm", "ckpt_frm")
	for _, r := range results {
		t.Logf("%-6d %-7s %-5d %12s %12.0f %12d %10d %10d",
			r.batchSize, r.syncMode, r.syncPragma, r.elapsed.Round(time.Millisecond),
			r.ratePerSec, r.walBytes, r.logFrames, r.ckptFrames)
	}

	// Surface the two headline ratios in-line so the close can quote them
	// without re-deriving from the table.
	base := results[0] // NORMAL@32
	for _, r := range results {
		if r.syncMode == "NORMAL" && r.batchSize > base.batchSize {
			t.Logf("batch %d vs %d (NORMAL): %.2fx drain speedup",
				r.batchSize, base.batchSize, base.elapsed.Seconds()/r.elapsed.Seconds())
		}
	}
	var norm256, full256 *armResult
	for i := range results {
		if results[i].batchSize == 256 && results[i].syncMode == "NORMAL" {
			norm256 = &results[i]
		}
		if results[i].batchSize == 256 && results[i].syncMode == "FULL" {
			full256 = &results[i]
		}
	}
	if norm256 != nil && full256 != nil {
		t.Logf("WAL fsync share @256: FULL is %.2fx NORMAL's drain time (close to 1.0 => fsync not the ceiling)",
			full256.elapsed.Seconds()/norm256.elapsed.Seconds())
	}
}

func runDrainArm(t *testing.T, dir string, n, batchSize int, syncMode string, gomaxprocs int) armResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tmpDir := t.TempDir()
	dbPath := tmpDir + "/veska.db"
	// BackupDir under the temp dir: keep migration snapshots out of the real
	// ~/.veska so the sweep never touches a live install.
	if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: tmpDir + "/backups"}); err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	defer func() { _ = pools.Close() }()

	// The DSN ships synchronous=NORMAL; the FULL arm overrides it on the single
	// write connection (write pool is MaxOpenConns(1), so the pragma sticks).
	if syncMode == "FULL" {
		if _, err := pools.Write.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
			t.Fatalf("set synchronous=FULL: %v", err)
		}
	}
	var syncPragma int
	if err := pools.Write.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncPragma); err != nil {
		t.Fatalf("read synchronous: %v", err)
	}

	seedPending(t, pools.Write, n)

	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.Write)
	provider, err := model2vec.New(dir)
	if err != nil {
		t.Fatalf("model2vec.New: %v", err)
	}
	vectors := memvec.New()
	workerOpts := []embedder.Option{
		embedder.WithBatchSize(batchSize),
		embedder.WithGovernor(embedder.NewFixedGovernor(gomaxprocs)),
	}
	// EMBED_METRICS=1 wires a real Metrics so the worker exercises the
	// per-pass EmbedQueueDepth gauge path (off by default in production and in
	// this harness) - used to A/B the gauge-throttle change.
	if os.Getenv("EMBED_METRICS") == "1" {
		workerOpts = append(workerOpts, embedder.WithMetrics(observability.NewMetrics(prometheus.NewRegistry())))
	}
	worker, err := embedder.NewWorker(refs, provider, vectors, workerOpts...)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	if pending, err := refs.CountPending(ctx); err != nil || pending != n {
		t.Fatalf("seed mismatch: pending=%d err=%v, want %d", pending, err, n)
	}

	start := time.Now()
	worker.Start(ctx)
	// Drain to empty: the cold-scan shape (finite queue), not a fixed window.
	for {
		pending, err := refs.CountPending(ctx)
		if err != nil {
			t.Fatalf("CountPending during drain: %v", err)
		}
		if pending == 0 {
			break
		}
		// Poll coarsely: CountPending is a state-filtered COUNT that competes with
		// the worker's classify reads, so a tight poll would confound the drain
		// time it is trying to measure.
		select {
		case <-ctx.Done():
			t.Fatalf("drain timed out at batch=%d sync=%s: %v", batchSize, syncMode, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	elapsed := time.Since(start)
	worker.Stop()
	worker.Wait()

	// WAL pressure: the -wal file size at end of drain (tail since the last
	// auto-checkpoint), then a PASSIVE checkpoint to read frames in flight.
	var walBytes int64
	if fi, err := os.Stat(dbPath + "-wal"); err == nil {
		walBytes = fi.Size()
	}
	var busy, logFrames, ckptFrames int
	_ = pools.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &ckptFrames)

	return armResult{
		batchSize: batchSize, syncMode: syncMode, syncPragma: syncPragma, n: n,
		elapsed: elapsed, ratePerSec: float64(n) / elapsed.Seconds(),
		walBytes: walBytes, logFrames: logFrames, ckptFrames: ckptFrames,
	}
}
