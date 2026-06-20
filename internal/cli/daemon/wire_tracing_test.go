// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"testing"
	"time"
)

// TestWire_TracingDisabledByDefault verifies that with tracing.enabled=false
// (the config default) the daemon constructs no TracerProvider (AC3).
func TestWire_TracingDisabledByDefault(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if d.tracerProvider != nil {
		t.Error("tracerProvider should be nil when tracing.enabled=false")
	}
}

// TestWire_TracingEnabled verifies that with tracing.enabled=true and an OTLP
// endpoint set the daemon constructs the TracerProvider and threads it into
// every tracing-aware consumer (AC1).
func TestWire_TracingEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.TracingEnabled = true
	cfg.TracingEndpoint = "127.0.0.1:4317" // never dialed: OTLP exporter dials lazily

	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	if d.tracerProvider == nil {
		t.Fatal("tracerProvider should be constructed when tracing.enabled=true")
	}
	if d.ingester.TracerProvider() == nil {
		t.Error("Ingester did not receive a TracerProvider")
	}
	if d.promoter.TracerProvider() == nil {
		t.Error("Promoter did not receive a TracerProvider")
	}
	if d.mcpReg.TracerProvider() == nil {
		t.Error("MCP Registry did not receive a TracerProvider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Stop must shut the TracerProvider down cleanly (flush + exporter close).
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestWire_TracingEnabledWithoutEndpoint verifies that enabling tracing with
// no endpoint is a fatal startup error (AC2).
func TestWire_TracingEnabledWithoutEndpoint(t *testing.T) {
	cfg := testConfig(t)
	cfg.TracingEnabled = true
	cfg.TracingEndpoint = ""

	if _, err := newDaemon(cfg); err == nil {
		t.Fatal("newDaemon should fail when tracing is enabled without an endpoint")
	}
}

// TestWire_TracingEndpointWithoutEnabled verifies that setting an endpoint
// while tracing is disabled is a fatal startup error (AC2).
func TestWire_TracingEndpointWithoutEnabled(t *testing.T) {
	cfg := testConfig(t)
	cfg.TracingEnabled = false
	cfg.TracingEndpoint = "127.0.0.1:4317"

	if _, err := newDaemon(cfg); err == nil {
		t.Fatal("newDaemon should fail when an endpoint is set with tracing disabled")
	}
}
