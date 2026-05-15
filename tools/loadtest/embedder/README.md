# embedder — M3 gate-1 throughput bench

Drives the real `embedder.Worker` against a local Ollama instance for a
measurement window and asserts sustained throughput is at or above the M3
gate-1 floor (5 emb/s; see `docs/design/M3.md`).

## What passing means

Over the configured window, the worker drained `embeds_completed` pending refs
from `node_embedding_refs`, giving a sustained rate at or above
`EMBED_BENCH_GATE_MIN_RATE`. The bench skips with a clear message if Ollama
is not reachable.

## What this actually measures

The **worker's** sustained output rate — i.e. the min of (Ollama capacity,
worker rate limiter). The worker defaults to 10 emb/s; the gate floor is
5 emb/s. A healthy local Ollama reports a rate near the limiter cap. A rate
significantly below the cap points at Ollama or model load time, not the
worker. We intentionally do NOT disable the limiter — production sees the
limiter, so the gate does too.

## Run

```bash
make eval-embed-throughput
```

## Knobs

| Env var | Default | Meaning |
|---|---|---|
| `EMBED_BENCH_DURATION_S` | `60` | Measurement window in seconds. |
| `EMBED_BENCH_SEED_N` | `2000` | Pending refs to seed. Must exceed `duration_s * 20` with headroom so the worker stays fed. |
| `EMBED_BENCH_GATE_MIN_RATE` | `5.0` | Floor (emb/s) below which the gate fails. |
| `VESKA_OLLAMA_URL` | `http://localhost:11434` | Ollama base URL. |
| `VESKA_EMBED_MODEL` | `nomic-embed-text` | Embedding model name. |

## Output

JSON is written to stdout (prefixed `EMBED_BENCH `) and to `t.Log`:

```json
{
  "model": "nomic-embed-text",
  "ollama_url": "http://localhost:11434",
  "seed_n": 2000,
  "duration_s": 60,
  "embeds_completed": 480,
  "rate_per_sec": 8.0,
  "gate_min_rate": 5.0,
  "gate_met": true
}
```
