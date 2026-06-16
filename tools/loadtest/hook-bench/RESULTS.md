# Hook p95 Benchmark - sendSeal round-trip

Generated: 2026-05-13
Platform: Linux amd64 (Intel Core i7-7700 @ 3.60GHz)
Socket: Unix domain (loopback)
Iterations: 500

| Metric | Value | Gate |
|--------|-------|------|
| p95 latency | 0.116ms | ≤100ms |
| p99 latency | 0.208ms | - |
| min | 0.033ms | - |
| max | 1.696ms | - |

Gate: PASS (p95 ≤ 100ms)

## Notes
- Measures hook shim only (dial + write {"cmd":"promote"} + read {"ok":true})
- Mock daemon reads the request line before responding (simulates real protocol)
- Daemon-side promotion work (50-file SQLite tx) measured separately in m1.08 exit-gate bench
