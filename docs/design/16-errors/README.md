---
id: SOLO-16
title: "Error Catalogue — veska_code, exit codes, audit shape"
status: draft
version: 0.1.0
last_reviewed: 2026-05-09
related: [SOLO-03, SOLO-08, SOLO-09, SOLO-10, SOLO-13]
---

# SOLO-16 — Error Catalogue

Every refusal, every refuse-to-start condition, every MCP error
response shares one shape. This file is the catalogue: code,
message template, exit code (where applicable), remediation
hint, and audit-line shape.

## 1. Why one catalogue

Error shapes are otherwise scattered across SOLO-03 §5.8
(refuse-to-start), SOLO-09 §4.6 (MCP error envelope), SOLO-10 §3
(human-action gate), SOLO-11 §10 (writer-pool busy), and a
handful of ad-hoc strings. New codes show up in M2/M3 and the
shape drifts. One catalogue, owned here; other docs cite a code
rather than re-define it.

## 2. The shared shape

### 2.1 MCP error envelope

JSON-RPC error returned over the socket:

```jsonc
{
  "error": {
    "code":    -32001,             // JSON-RPC code; see §3
    "message": "<short, stable>",  // human-readable summary
    "data": {
      "veska_code": "ErrXxx",     // stable string id; the catalogue key
      "context":     { /* code-specific payload */ }
    }
  }
}
```

The `veska_code` is the contract. The `message` is friendly
prose that may improve over time. Tooling MUST key on
`veska_code`, not `message`.

### 2.2 CLI exit codes

| Exit | Meaning |
|---|---|
| 0 | Success. |
| 1 | Degraded — work completed but the result is partial or stale. `veska doctor` reports `status: degraded`. |
| 2 | Broken — work failed; the user must remediate. `veska doctor` reports `status: broken`. |
| 78 | Refuse-to-start (daemon only). The supervisor halts; not retry-eligible. |

### 2.3 Audit-line shape

```jsonc
{
  "v": 1,
  "ts": "...",
  "actor_id": "...",
  "actor_kind": "...",
  "tool": "<verb>",
  "args": { /* ... */ },
  "result": "refused: <veska_code>"   // or "error: <veska_code>" for handler errors
}
```

The `result` field carries the `veska_code` directly. SOLO-08
§3.5 owns the schema and stability rules for the line itself.

## 3. The catalogue

Codes are grouped by surface. Within each group, every code
is paired with: where it fires, the JSON-RPC code (for MCP
surfaces), the exit code (for CLI surfaces), the `context`
payload, and the remediation.

### 3.1 Daemon refuse-to-start (exit 78)

Canonical home: SOLO-03 §5.8. Every code below maps to a row
in that matrix.

| `veska_code` | When | Remediation |
|---|---|---|
| `ErrCrashLoop` | `~/.veska/state/broken` marker present at start | `veska doctor reset-crash-loop` after investigation |
| `ErrSqliteVecMissing` | sqlite-vec extension missing or unloadable | reinstall the binary (extension is bundled per SOLO-08 §1.1); if persistent, file a bug |
| `ErrSchemaTooNew` | `current < min_schema` | downgrade binary, or restore newer backup |
| `ErrSchemaTooOld` | `current > max_schema` | upgrade binary, or restore pre-upgrade backup |
| `ErrMigrationFailed` | Migration N rolled back | fix migration / downgrade / restore pre-migration snapshot |
| `ErrSnapshotFailed` | Pre-migration auto-snapshot failed | free disk; fix permissions; restart |
| `ErrMigrationTampered` | `migration_sha` recorded ≠ binary's embedded sha | investigate; do not blindly clear |
| `ErrEmbedderMismatch` | `[embedder]` config disagrees with `database_meta` | `veska embedder swap <model>`, or revert config |
| `ErrUnsupportedFilesystem` | `~/.veska/` on NFS, eCryptfs, FUSE, or overlay-upper | move data dir; set `VESKA_HOME` |
| `ErrBackupRequired` | `[backup].required = true` and no verified backup found | `veska backup create`; restart |

JSON-RPC code: N/A (these never reach the wire — the daemon never came up).
Audit line: N/A (the daemon never opened the audit log).
Surface: stderr + the supervisor's exit code log.

### 3.2 Daemon runtime (breaker-eligible, non-78)

| `veska_code` | When | Behaviour |
|---|---|---|
| `ErrRSSExceeded` | RSS > `[memory].hard_cap_gib` (4 GiB DEFAULT) | Process exits non-zero; supervisor restarts; counts against breaker (SOLO-03 §5.6) |
| `ErrCoreGoroutinePanic` | Panic in promotion / MCP router / watcher post-start | Same |

Audit line: not written for the panic (the daemon is dying). Crash details land in `~/.veska/logs/daemon.log` and the breaker notification path (SOLO-03 §5.6).

### 3.3 MCP surface (JSON-RPC errors)

| `veska_code` | JSON-RPC | When | `context` payload | Remediation |
|---|---|---|---|---|
| `ErrDaemonNotRunning` | -32000 | Shim cannot reach socket and no supervisor is registered | `{"cli_command": "veska service install"}` | Install the service |
| `ErrDaemonStarting` | -32000 | Write tool called during startup-resync | `{"resync_state": "running"}` | Wait; resync will complete |
| `ErrHumanActionRequired` | -32001 | High-severity close from non-human actor (SOLO-10 §3) | `{"gate": "close.finding.high", "finding_id": "...", "severity": "...", "cli_command": "veska finding close ... --reason \"...\""}` | Human pastes the CLI command |
| `ErrBusy` | -32002 | MCP write `max_wait_ms` deadline expired (SOLO-11 §10) | `{"cause": "seal_in_flight" \| "seal_arriving" \| "pool_wait", "promotion_id"?: "...", "wait_count"?: N, "wait_duration_ms"?: N, "eta_ms"?: N}` | Retry; raise `max_wait_ms` |
| `ErrRepoNotRegistered` | -32003 | Tool called with a `repo` that is not in `repos` | `{"repo_id_or_path": "..."}` | `veska repo add <path>` |
| `ErrInvalidArgs` | -32602 | JSON-RPC standard; argument schema violation | `{"field": "...", "reason": "..."}` | Fix the call |
| `ErrInternal` | -32603 | Unhandled handler panic; logged as a defect | `{"trace_id": "..."}` | File a bug with the trace ID |

Audit line: every refusal and every handler error is logged synchronously per SOLO-10 §4. `result` carries `"refused: <veska_code>"` or `"error: <veska_code>"`.

### 3.4 Pipeline / async work

| `veska_code` | When | Surface |
|---|---|---|
| `ErrPromotionDivergent` | `last_promoted_sha` not reachable from `HEAD` (force-push, history rewrite) | Logged at startup-resync (SOLO-03 §5.7) and during catch-up replay (SOLO-11 §2.3); not user-blocking |
| `ErrEmbedDeferred` | `post_promotion_queue` over high-water; embed rows insert as `state='deferred'` | `degraded_reasons:["post_promotion_queue_deferred:embed:<count>"]` on subsequent reads (SOLO-08 §3.4) |
| `ErrEmbedFailed` | Embed row exhausted retries | `degraded_reasons:["embedding_failed"]`; `veska doctor post-promotion-queue retry --kind=embed` |
| `ErrReviewBudgetExceeded` | Per-commit token cap hit | Sticky finding `review-pipeline-budget-exceeded`, severity medium |
| `ErrReviewPipelineFailure` | Review row exhausted retries | Sticky finding `review-pipeline-failure`, severity high; closes through the human-action gate |
| `ErrParseFailure` | Tree-sitter parse error on a file | Finding `rule='parse-failure'`, `source_layer='structural'`; the promotion proceeds |
| `ErrEmbedSaturated` | Deferred-embed queue's oldest row aged past 24h | Sticky finding `embed-deferred-saturated`, severity medium |

These are not MCP errors — they are *findings* or *degraded reasons*. The `veska_code` keys are stable so tooling can correlate.

### 3.5 Backup, restore, embedder swap

| `veska_code` | When | Exit | Remediation |
|---|---|---|---|
| `ErrBackupVerifyFailed` | `veska backup verify` integrity_check or FK check failed | 2 | The backup is unusable; create a new one |
| `ErrBackupCorrupt` | `veska doctor backup` finds the most recent backup unreadable | 2 | Same |
| `ErrRestoreDaemonRunning` | `veska backup restore` while daemon up | 2 | `veska daemon stop`; rerun |
| `ErrRestorePartial` | Restore failed mid-sequence; rolled back via `.replaced-<ts>/` sidecar | 3 | Sidecar preserved; investigate before retrying |
| `ErrEmbedderSwapInconsistent` | `database_meta.embedder_*` ≠ live `vec_nodes` declared dim at start | 78 | Restore most recent `pre-swap-*` snapshot |
| `ErrEmbedderModelMissing` | Pre-swap probe fails | 1 | `ollama pull <model>`; retry |

### 3.6 Filesystem / disk

| `veska_code` | When | Surface |
|---|---|---|
| `ErrDiskLow` | `veska doctor storage` free < 1 GiB | exit 1 (degraded); `messages[].code = "disk_low"` |
| `ErrDiskFull` | free < 100 MiB | exit 2 (broken); promotion returns `ErrDiskFull`; `degraded_reasons:["disk_full"]` |
| `ErrInotifyBudget` | `inotify_add_watch` returns `ENOSPC` | warn-level log; affected repo flips to polling fallback (SOLO-03 §3.0) |
| `ErrFSEventsBudget` | macOS FSEvents per-mount budget exceeded | warn-level log; `veska doctor watcher` recommends polling |

## 4. Stability and additions

The catalogue follows the same discipline as SOLO-08 §3.5's `v`
bump rule:

| Change | Bumps anything? |
|---|---|
| Adding a new `veska_code` | no — additive; tooling tolerates unknowns |
| Adding an optional field to `context` | no |
| Removing or renaming an `veska_code` | yes — minor version bump + CHANGELOG note |
| Repurposing an existing `veska_code` | yes |
| Changing the type of a `context` field | yes |

Tooling MUST tolerate unknown `veska_code` values (forward
compat) by treating them as opaque error markers.

## 5. Cross-references

- SOLO-03 §5.6, §5.8 — the refuse-to-start matrix and
  breaker-eligible exits.
- SOLO-09 §4.6 — the JSON-RPC error envelope on the wire.
- SOLO-10 §3 — the human-action gate and `ErrHumanActionRequired`.
- SOLO-11 §10 — `ErrBusy` and the promotion barrier.
- SOLO-13 §2 — `veska doctor` exit codes and `--json` schema.
- SOLO-08 §3.5 — the audit-line schema this catalogue's
  `result` strings target.
