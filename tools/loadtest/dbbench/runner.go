// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

package dbbench

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// WorkloadResult is per-workload, per-driver timing summary.
type WorkloadResult struct {
	Workload string
	Driver   string
	Iters    int
	P50Ms    float64
	P95Ms    float64
	P99Ms    float64
	MaxMs    float64
	OpsPerS  float64
}

// RunConfig controls iteration counts. Defaults match the test harness; the
// test exposes env-var overrides so a deeper run can be triggered ad-hoc.
type RunConfig struct {
	WarmupIters   int
	GraphIters    int
	FTSIters      int
	QueueIters    int
	PromotionTx   int
	BulkBatchSize int
	BulkIters     int
	RehydrateRuns int
}

func DefaultRunConfig() RunConfig {
	return RunConfig{
		WarmupIters:   50,
		GraphIters:    2000,
		FTSIters:      1000,
		QueueIters:    1000,
		PromotionTx:   500,
		BulkBatchSize: 500,
		BulkIters:     50,
		RehydrateRuns: 5,
	}
}

// Run executes every workload against an already-opened+seeded Bench.
func Run(ctx context.Context, b Bench, cfg RunConfig) ([]WorkloadResult, error) {
	out := make([]WorkloadResult, 0, 6)

	graph := func(i int) error { return b.GraphRead(ctx, i) }
	if err := warmup(graph, cfg.WarmupIters); err != nil {
		return nil, fmt.Errorf("warmup graph_read: %w", err)
	}
	out = append(out, time1(b.Name(), "graph_read", cfg.GraphIters, graph))

	fts := func(i int) error { return b.FTSQuery(ctx, i) }
	if err := warmup(fts, cfg.WarmupIters); err != nil {
		return nil, fmt.Errorf("warmup fts_query: %w", err)
	}
	out = append(out, time1(b.Name(), "fts_query", cfg.FTSIters, fts))

	queue := func(i int) error { return b.QueuePoll(ctx, i) }
	if err := warmup(queue, cfg.WarmupIters); err != nil {
		return nil, fmt.Errorf("warmup queue_poll: %w", err)
	}
	out = append(out, time1(b.Name(), "queue_poll", cfg.QueueIters, queue))

	prom := func(i int) error { return b.PromotionTx(ctx, i) }
	if err := warmup(prom, cfg.WarmupIters/5); err != nil {
		return nil, fmt.Errorf("warmup promotion_tx: %w", err)
	}
	out = append(out, time1(b.Name(), "promotion_tx", cfg.PromotionTx, prom))

	bulk := func(i int) error { return b.BulkIngest(ctx, cfg.BulkBatchSize, i) }
	out = append(out, time1(b.Name(), "bulk_ingest", cfg.BulkIters, bulk))

	rehy := func(i int) error {
		_, err := b.RehydrateScan(ctx)
		return err
	}
	out = append(out, time1(b.Name(), "rehydrate_scan", cfg.RehydrateRuns, rehy))
	return out, nil
}

func warmup(fn func(int) error, n int) error {
	for i := 0; i < n; i++ {
		if err := fn(i); err != nil {
			return err
		}
	}
	return nil
}

func time1(driver, name string, iters int, fn func(int) error) WorkloadResult {
	samples := make([]time.Duration, 0, iters)
	start := time.Now()
	for i := 0; i < iters; i++ {
		t0 := time.Now()
		if err := fn(i); err != nil {
			// Record but keep going; an error mid-run will show as zero
			// elapsed and we'll see it in the iteration count vs samples.
			fmt.Printf("[%s/%s] iter %d error: %v\n", driver, name, i, err)
			continue
		}
		samples = append(samples, time.Since(t0))
	}
	total := time.Since(start)
	return summarize(driver, name, iters, samples, total)
}

func summarize(driver, name string, iters int, samples []time.Duration, total time.Duration) WorkloadResult {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	pct := func(p float64) float64 {
		if len(samples) == 0 {
			return 0
		}
		idx := int(float64(len(samples)-1) * p)
		return float64(samples[idx].Microseconds()) / 1000.0
	}
	max := 0.0
	if len(samples) > 0 {
		max = float64(samples[len(samples)-1].Microseconds()) / 1000.0
	}
	ops := 0.0
	if total > 0 {
		ops = float64(len(samples)) / total.Seconds()
	}
	return WorkloadResult{
		Workload: name,
		Driver:   driver,
		Iters:    iters,
		P50Ms:    pct(0.50),
		P95Ms:    pct(0.95),
		P99Ms:    pct(0.99),
		MaxMs:    max,
		OpsPerS:  ops,
	}
}
