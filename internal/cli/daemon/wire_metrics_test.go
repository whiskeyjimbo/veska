// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestWire_MetricsDisabledByDefault verifies that with metrics.enabled=false
// (the config default) the daemon constructs no Metrics and binds no HTTP
// listener (AC2).
func TestWire_MetricsDisabledByDefault(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if d.metrics != nil {
		t.Error("metrics should be nil when metrics.enabled=false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.metricsCloser != nil {
		t.Error("metrics HTTP listener should not be bound when metrics.enabled=false")
	}
}

// TestWire_MetricsEnabled verifies that with metrics.enabled=true the daemon
// constructs a Metrics, binds the HTTP listener on the configured address, and
// threads the Metrics into the daemon-wired consumers (AC1 + AC3).
func TestWire_MetricsEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.MetricsEnabled = true
	cfg.MetricsListen = "127.0.0.1:0" // OS-assigned free port

	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if d.metrics == nil {
		t.Fatal("metrics should be constructed when metrics.enabled=true")
	}
	if d.metrics.ErrorCount == nil {
		t.Error("metrics.ErrorCount should be registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.metricsCloser == nil {
		t.Fatal("metrics HTTP listener should be bound when metrics.enabled=true")
	}
	if d.metricsAddr == "" {
		t.Fatal("metrics listener address should be recorded")
	}
	// The listener should be reachable.
	conn, derr := net.DialTimeout("tcp", d.metricsAddr, 2*time.Second)
	if derr != nil {
		t.Fatalf("metrics listener not reachable at %q: %v", d.metricsAddr, derr)
	}
	_ = conn.Close()

	// Stop must shut the listener down: a subsequent dial should fail.
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if conn, derr := net.DialTimeout("tcp", d.metricsAddr, 500*time.Millisecond); derr == nil {
		_ = conn.Close()
		t.Error("metrics listener still reachable after Stop")
	}
}

// TestMetricsErrorCounter verifies the review ErrorCounter adapter increments
// Metrics.ErrorCount (AC3).
func TestMetricsErrorCounter(t *testing.T) {
	cfg := testConfig(t)
	cfg.MetricsEnabled = true
	cfg.MetricsListen = "127.0.0.1:0"
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	adapter := metricsErrorCounter{m: d.metrics}
	adapter.IncError("review")

	if got := testutil.ToFloat64(d.metrics.ErrorCount.WithLabelValues("review")); got != 1 {
		t.Errorf("ErrorCount{kind=review} = %v; want 1", got)
	}
}
