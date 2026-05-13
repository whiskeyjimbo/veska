# M1 Exit-Gate Report

Generated: 2026-05-13

| Gate | Budget | Measured | Verdict |
|------|--------|----------|---------|
| 1. Cold-scan 100k LOC | <60s | 1.616s | PASS |
| 2. find_symbol warm p95 | <50ms | — | PENDING (m1.08.3) |
| 3. Post-commit hook p95 | <100ms | 0.116ms | PASS |
| 4. Daemon RSS | ≤2GiB | — | PENDING (m1.08.5) |
| 5. Promotion 50k-symbol refactor | <5s p95 | — | PENDING (m1.08.5) |
| 6. semantic_search p95 @50k vectors | <100ms | 1.90ms | PASS |
| 7. All tests pass -race | all pass | see CI | PENDING |
| 8. golangci-lint + layercheck clean | clean | see CI | PENDING |
