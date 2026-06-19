// SPDX-License-Identifier: AGPL-3.0-only

package hookcmd

import (
	"bufio"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// BenchmarkSendSeal measures the round-trip latency of the hook shim
// against a mock Unix socket server that replies {"ok":true} immediately.
func BenchmarkSendSeal(b *testing.B) {
	sockPath := filepath.Join(b.TempDir(), "mock.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Mock daemon: accept connections, read the request line, write ack.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				scanner.Scan() // consume {"cmd":"promote"}
				c.Write([]byte("{\"ok\":true}\n"))
			}(conn)
		}
	}()

	latencies := make([]time.Duration, 0, b.N)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		SendSeal(sockPath)
		latencies = append(latencies, time.Since(start))
	}
	b.StopTimer()

	// Compute and report percentiles.
	slices.Sort(latencies)

	p95idx := int(float64(len(latencies)) * 0.95)
	if p95idx >= len(latencies) {
		p95idx = len(latencies) - 1
	}
	p99idx := int(float64(len(latencies)) * 0.99)
	if p99idx >= len(latencies) {
		p99idx = len(latencies) - 1
	}

	p95ms := float64(latencies[p95idx]) / float64(time.Millisecond)
	p99ms := float64(latencies[p99idx]) / float64(time.Millisecond)
	minMs := float64(latencies[0]) / float64(time.Millisecond)
	maxMs := float64(latencies[len(latencies)-1]) / float64(time.Millisecond)

	b.ReportMetric(p95ms, "p95_ms")
	b.ReportMetric(p99ms, "p99_ms")
	b.ReportMetric(minMs, "min_ms")
	b.ReportMetric(maxMs, "max_ms")
}
