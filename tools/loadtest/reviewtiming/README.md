# reviewtiming — M5 exit-gate-5 per-commit review time budget

Measures the wall-clock time to run **one review pass over a ~100-file commit**
using the real review `Handler` (the `WorkKindReview` lane) wired to a real
local Ollama generator.

This is a **measurement harness**, not a pass/fail gate. The reference-laptop
number it produces is recorded by a human in `docs/milestones/M5.md` (M5 exit
gate 5).

## What it measures

For each of `REVIEW_TIMING_FILE_N` synthetic Go source files, the harness
dispatches one `WorkKindReview` row through `review.Handler.Handle`, which
renders every registered review prompt over the file and calls the Ollama LLM.
It reports:

- `total_ms` / `wall_clock_budget` — the total time for the 100-file pass.
- `per_file_mean_ms` — `total_ms / file_n`.
- `files_reviewed_ok` / `files_failed`.

The fixture is generated in a temp directory (simple distinct Go files); no
real corpus or repo is required.

## Run

```bash
make eval-review-timing
```

The harness **skips cleanly** (`t.Skip`) when Ollama is unreachable *or* when
the configured review model is not pulled, so it is CI-safe and verifiable
without a model. To run it for real, pull a generation model first
(`ollama pull llama3`).

## Knobs

| Env var | Default | Meaning |
|---|---|---|
| `REVIEW_TIMING_FILE_N` | `100` | Number of synthetic files in the commit fixture. |
| `VESKA_OLLAMA_URL` | `http://localhost:11434` | Ollama base URL. |
| `VESKA_REVIEW_MODEL` | `llama3` | Review LLM model name. |
| `REVIEW_TIMING_LLM_TIMEOUT` | `5m` | Per-call LLM timeout. The llm-package default (60s) is too tight for a structured-JSON review generation on CPU Ollama; this generous default avoids per-call deadline failures. Accepts a Go duration (e.g. `10m`). |

> **Note on the overall test timeout.** A full 100-file pass on CPU Ollama can
> run for a long time (per-file latency × `REVIEW_TIMING_FILE_N`). For a quick
> first measurement, run with a smaller `REVIEW_TIMING_FILE_N` (e.g. 10) and
> multiply, or raise the `-timeout` on the `make eval-review-timing` target.

## Output

JSON is written to stdout (prefixed `REVIEW_TIMING `) and to `t.Log`:

```json
{
  "model": "llama3",
  "ollama_url": "http://localhost:11434",
  "file_n": 100,
  "total_ms": 0,
  "per_file_mean_ms": 0,
  "files_reviewed_ok": 100,
  "files_failed": 0,
  "wall_clock_budget": "0s"
}
```
