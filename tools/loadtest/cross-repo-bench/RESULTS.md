# Cross-Repo p95 Bench - ResolveStubsForNode

Generated: 2026-05-13
Platform: linux amd64
Repos: 2 (repo-service, repo-sdk)
Nodes per repo: 1000
Cross-repo stubs: 200
Runs: 1000

## Results

| Metric | Value |
|--------|-------|
| p95 latency | 339µs |
| Verdict | GREEN |

## Thresholds

| Color | Threshold |
|--------|-----------|
| GREEN  | p95 < 50ms |
| YELLOW | 50ms ≤ p95 < 150ms |
| RED    | p95 ≥ 150ms |

## OQ-S010 Resolution

OQ-S010 asked: is per-hop cross-repo resolve latency acceptable for interactive use?

Measured p95 = 339µs - **GREEN**

Result is within the GREEN threshold (< 50ms p95). OQ-S010 is **RESOLVED - no cache ADR required before M2**.
