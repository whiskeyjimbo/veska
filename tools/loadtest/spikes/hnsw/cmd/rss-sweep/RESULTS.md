# RSS and Scale Sweep: UsearchStore (float16)

Generated: 2026-05-13 03:44:37 UTC

Corpus: 768-dim synthetic vectors, seed=42 (in index).
Queries: 200 warm queries, seed=999 (hold-out, not in index). k=10.
RSS measured via `/proc/self/status` `VmRSS` on Linux.

## Results

| Population | Build Time | RSS at Load | RSS Steady-State | Warm P95 (ms) | Budget (1.5GiB) |
|-----------|-----------|------------|-----------------|--------------|----------------|
| 50k | 110.3s | 103MiB | 104MiB | 1.43 | n/a |
| 250k | 872.6s | 504MiB | 505MiB | 3.57 | n/a |
| 500k | 2123.0s | 1015MiB | 1016MiB | 15.55 | n/a |
| 1000k | 4657.5s | 2023MiB | 2023MiB | 2.59 | ✗ EXCEEDS |

## Notes

- All measurements on Linux amd64 (same host as ADR-S0014 application-layer bench).
- float16 quantization (production default).
- Budget threshold: 1.5 GiB (1536 MiB) RSS at 1M vectors steady-state.
