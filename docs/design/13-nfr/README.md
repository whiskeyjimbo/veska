---
id: SOLO-13
title: "NFR — Observability, Performance, Degraded Modes, CI"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-08, SOLO-11, SOLO-14]
verified: true
verified_date: "2026-05-16"
---

# SOLO-13 — Non-Functional Requirements

One developer, one daemon, one machine. The NFR surface tracks that
shape: structured logs by default, everything else opt-in, one
operator command (`veska doctor`), and performance numbers
labelled per the §3 convention (`BUDGET (unmeasured)`,
`BUDGET (measured M<N>)`, `INVARIANT`, `DEFAULT`).

## 1. Observability

### 1.1 Logs

- **slog** is the only logging library. Always on. Default handler
  is text on stderr; `VESKA_LOG_FORMAT=json` switches to JSON.
- Default level is `info`. `VESKA_LOG_LEVEL=debug` raises it.
- Standard attribute names (used consistently, not lint-enforced):
  `repo_id`, `branch`, `node_id`, `actor_id`, `actor_kind`, `tool`,
  `promotion_id`, `work_kind`, `error`.
- No PII or secret content in log values. Free-form messages stay
  short; structured data goes in attrs.
- Logs go to stderr. Forward with `journalctl`, `tail`, or a
  container runtime. No built-in shipper.

### 1.2 Metrics

Prometheus `/metrics` is **opt-in**. Set `metrics.enabled = true` in
`config.toml` or `VESKA_METRICS_LISTEN=127.0.0.1:9090`. When off, no HTTP
listener is bound.

**Port collision behavior.** The default `127.0.0.1:9090` is the
canonical Prometheus port and collides with a running Prometheus,
Cockroach UI, and several other dev tools. On `bind: address
already in use`, the daemon does **not** silently skip metrics —
it writes a `warn`-level slog line and surfaces a `degraded`
status in `veska doctor metrics` (exit 1) with the bound port
recovered from the OS error and a suggested alternative
(`127.0.0.1:9091`). The user picks a free port in
`metrics.listen` and restarts. We do not auto-probe-and-rebind:
the port is part of the operator's mental model and silent
movement breaks scrape configs.

The metric set is the spec, not a starting point. Twelve series:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `veska_seal_latency_seconds` | histogram | `repo_id` | End-to-end promotion duration: hook entry → SQL commit → post-promotion queue enqueue. |
| `veska_post_commit_hook_duration_seconds` | histogram | `repo_id`, `commit_size` (`typical` \| `refactor`) | Wall-clock from hook entry to hook return — the user-visible commit latency budget (SOLO-13 §3.1). |
| `veska_post_promotion_queue_depth` | gauge | `work_kind`, `state` | Rows in `post_promotion_queue` by `(work_kind, state)`. Replaces the prior pending-only gauge; covers `embed`/`auto_link`/`revalidate`/`review` × `pending`/`running`/`failed`. |
| `veska_post_promotion_queue_oldest_pending_seconds` | gauge | `work_kind` | Age of the oldest `pending` row per work_kind. Surfaces embed lag during catch-up. |
| `veska_writer_pool_busy_timeout_total` | counter | `pool` (`hot` \| `embed`) | Times `BEGIN IMMEDIATE` hit `busy_timeout` waiting for the SQLite OS lock. Catches embed/hot contention (ADR-S0011). |
| `veska_writer_pool_wait_seconds` | histogram | `pool` | Time MCP writes spent waiting for an available connection in `writeDB.hot` / `writeDB.embed`. |
| `veska_mcp_requests_total` | counter | `tool`, `result` | MCP tool call count. `result` is `ok`, `error`, or `degraded`. |
| `veska_mcp_request_duration_seconds` | histogram | `tool`, `result` | MCP tool handler duration. |
| `veska_vector_query_duration_seconds` | histogram | `kind` (`semantic_search` \| `find_similar_symbols`) | sqlite-vec ANN query latency. The number that decides whether vec0 is still on-budget at the current node count (ADR-S0001). |
| `veska_daemon_memory_rss_bytes` | gauge | — | Process RSS from `/proc/self/status`. |
| `veska_daemon_uptime_seconds` | counter | — | Seconds since daemon start. |
| `veska_error_count` | counter | `kind` | Errors by kind (`promotion`, `embed`, `mcp`, `parse`, `watcher`). |
| `veska_disk_free_bytes` | gauge | `mount` | Free bytes on the filesystem hosting `~/.veska/`. Catches disk pressure before it surfaces as a write failure. |
| `veska_resync_in_flight` | gauge | `repo_id` | 1 while `last_promoted_sha < HEAD` replay is running for the repo, 0 otherwise. Lets the operator see startup-resync dominating cold start. |
| `veska_branch_row_count` | gauge | `table` (`nodes` \| `edges` \| `findings`) | Row count per per-branch-PK table. Surfaces the OQ-S006 branch-PK growth concern at runtime instead of leaving it spike-only. |

No SLO definitions, burn-rate pages, or alert routing ship in
the daemon. Operators who want those scrape `/metrics` into
their own stack.

### 1.3 Traces

OTLP traces are **opt-in and off by default**. They stay
unconfigured unless the user explicitly sets both
`tracing.enabled = true` *and* `VESKA_OTLP_ENDPOINT=...`.
Setting one without the other is a config error caught at
startup. There is no default endpoint and no default sampler
ratio: when the operator opts in, they pick both. The pillar
"zero telemetry by default" (§3 of SOLO-01) means traces never
leave the daemon unless the operator wired both knobs
deliberately. When enabled, the recommended sampler is
`parentbased_traceidratio` at 1.0 — for a single-user product,
fractional sampling makes traces useless when the user actually
looks; the user lowers it via `[tracing] sample_ratio` if their
collector is overwhelmed.

Spans we emit:

- `promotion.transaction` — the SQLite write transaction.
- `mcp.<tool>` — one per MCP handler invocation.
- `embed.run` — one per embedder batch.
- `parse.file` — one per tree-sitter parse.

Span attributes mirror log attribute names. No proprietary
trace exporters; OTLP only.

## 2. Health

There is one health surface: `veska doctor`. The CLI presents
**three** commands; everything else is structured data inside
the JSON output.

**Exit codes (canonical table).** Three independent regimes;
scripts must know which command they're consuming.

| Command | Exit | Meaning |
|---|---|---|
| `veska doctor`, `veska doctor --json` | 0 | healthy |
| | 1 | degraded (any section non-`ok` and non-broken) |
| | 2 | broken (any section reports broken; worst-of) |
| `veska doctor fix` | 0 | every action succeeded or skipped cleanly |
| | 1 | partial (one or more actions returned degraded) |
| | 2 | failed (one or more actions errored) |
| `veska-daemon` (start path) | 0 | clean shutdown |
| | non-78, non-zero | runtime crash; supervisor restarts; counts toward §5.6 breaker |
| | 78 | refuse-to-start (§5.8 row); supervisor halts; **does not** count toward breaker |

The breaker exits in §5.6 set the `~/.veska/state/broken`
marker so subsequent CLI invocations see the banner; the §5.8
refuse-to-start path also writes the marker (with a different
remediation hint) per SOLO-03 §5.6.

```
veska doctor              # human-readable summary; runs every check, prints a digest
veska doctor fix          # auto-remediates what it can; see §2.1a for the contract
veska doctor --json       # full machine-readable bundle, all sections, in one object
                           # (used by `veska doctor bundle`, by editor integrations,
                           # and by anyone scripting against doctor output)
```

The bare summary digest aggregates every section the JSON output
produces; users who want to see one part run
`veska doctor --json | jq '.data.<section>'`. We do not ship
13 subcommands for the 13 sections — every one of them collected
maintenance burden out of proportion to a one-developer surface.

Exit code: 0 on healthy, 1 on degraded, 2 on broken. The
`--json` schema is normative (§2.1); operators and tooling
consume it as a stable contract.

Two adjacent CLI verbs that are *not* `veska doctor`
subcommands (because they take action, not report state):

```
veska bundle              # writes a sharable diagnostic tarball (§2.2)
veska restore --pre-migration   # restores the most recent pre-migration auto-snapshot
                                  # (SOLO-08 §10.3)
```

The exit code is the worst across all sections. Per-section
status (and `code` strings for tooling) is in the `--json`
output's `data.<section>.status` and `data.<section>.code`
fields. Section codes are listed in §2.1.1–§2.1.10 below.

There are **no HTTP health endpoints**. No `/livez`, `/readyz`,
`/healthz`. K8s probes are not in scope; this daemon does not
run in K8s. If `metrics.enabled = true`, the same listener
serves `/metrics` and nothing else.

`veska doctor` is the user-visible operator surface. Spec it
before its observability backend; everything else feeds it.

**Section milestone map.** Not every section ships at M1; each
lands with the feature it covers. The mapping is:

| Section | First shipped at | Carrier feature |
|---|---|---|
| `status`, `egress`, `storage`, `embedder`, `config` | M1 | m1.07 doctor & audit |
| `identity` | M2 | m2.01 actor-on-every-write |
| `service`, `post_promotion_queue`, `pipelines` | M3 | m3.01–m3.05 (post-promotion queue + drain goroutines) |
| `backup` | M3 | first auto-snapshot landing (SOLO-08 §10) |

The bare `veska doctor` (no subcommand) reports only the
sections shipped in the current binary — it does not stub
unimplemented sections. Older binaries reporting a smaller set
is forward-compatible per §2.1's `data` tolerance rule.

### 2.1a `veska doctor fix`

`veska doctor fix` runs every remediation the daemon can
perform without ambiguity. The contract:

- **Actions taken without prompt** (idempotent, non-destructive):
  - Retry every `failed` row in `post_promotion_queue` (kind-agnostic;
    same path as `veska doctor post-promotion-queue retry`).
  - Reopen the daemon log file (`SIGHUP`-equivalent) if it was
    rotated externally.
  - Refresh stale vuln cache if `[vuln_source]` is configured.
- **Actions taken on confirmation** (interactive only; require
  `y` from a TTY):
  - Clear the crash-loop marker (`veska doctor reset-crash-loop`).
  - Write a fresh backup if `veska doctor backup` reports stale.
- **Never taken**: anything that mutates `nodes`, `edges`,
  `findings`, or schema; anything that calls Ollama's pull
  endpoint; anything that touches an unrelated repo.

**No-TTY behaviour.** When stdout is not a TTY (CI, script,
editor integration), `veska doctor fix` runs only the no-prompt
actions and exits 0. Confirmation-required actions are skipped
with a `messages[]` line listing the skipped action and the
explicit subcommand to run it manually. `--yes` overrides this
to take all actions; `--dry-run` reports what would happen
without taking any action.

**Partial failure.** Each action is independent. A failure in
one (e.g., backup write fails because disk is full) does not
abort the rest. The exit code is the worst across all attempted
actions: 0 if every action succeeded or was skipped cleanly, 1
if any action returned degraded, 2 if any action failed.
`--json` output enumerates per-action result.

`--json` shape:

```jsonc
"data": {
  "actions": [
    {"action": "retry_failed_post_promotion_queue", "ran": true,  "result": "ok",       "details": {"rows_retried": 3}},
    {"action": "reopen_log",                   "ran": true,  "result": "ok",       "details": null},
    {"action": "refresh_vuln_cache",           "ran": false, "result": "skipped",  "details": {"reason": "vuln_source not configured"}},
    {"action": "reset_crash_loop",             "ran": false, "result": "skipped",  "details": {"reason": "no marker present"}},
    {"action": "fresh_backup",                 "ran": false, "result": "skipped",  "details": {"reason": "no-tty: confirm-required action skipped; run `veska backup create`"}}
  ]
}
```

Codes: `ok`, `partial` (degraded), `failed` (broken).

### 2.1 `--json` output schema

`veska doctor [<subcmd>] --json` writes one JSON object to
stdout, then exits with the same code as the human-readable form.
The schema is **stable across patch releases**; additive
changes (new optional fields, new `messages[].code` values) do
not bump `schema_version`. Removing or renaming fields, changing
field types, or repurposing exit codes does — and ships only in
a minor version bump with a migration note.

**Common envelope.** Every `--json` response has this shape:

```jsonc
{
  "schema_version": 1,                          // bumps on breaking change
  "command":        "veska doctor storage",    // exact subcommand
  "ts":             "2026-05-09T18:42:31.123Z", // RFC 3339, UTC
  "veska_version": "v2.0.0-rc1",               // build version
  "host": {
    "os":     "linux",                          // GOOS
    "arch":   "amd64",                          // GOARCH
    "kernel": "6.8.0-111-generic"               // best-effort
  },
  "exit_code": 0,                               // 0|1|2
  "status":    "healthy",                       // "healthy"|"degraded"|"broken"
  "messages": [                                 // operator-facing notes
    {
      "level": "info",                          // "info"|"warn"|"error"
      "code":  "ok",                            // stable string id; see per-subcmd tables
      "text":  "free: 412 GiB; db: 1.2 GiB; WAL: 8 MiB; vec0: 240 MiB"
    }
  ],
  "data": {  /* per-subcommand payload — see §2.1.1–§2.1.10 */  }
}
```

The `data` field is the only varying portion. Operators and
tooling MUST tolerate unknown fields inside `data` (forward
compat) and unknown `messages[].code` values (parsers ignore
them rather than fail).

The bare `veska doctor --json` (no subcommand) returns
`status` set to the worst observed across all subcommands and
`data` set to a map of `{subcommand_name: <its full envelope>}`.

#### 2.1.1 `status`

```jsonc
"data": {
  "daemon_running":  true,
  "sockets": {
    "cli": {"path": "/home/jeff/.veska/cli.sock", "reachable": true},
    "mcp": {"path": "/home/jeff/.veska/mcp.sock", "reachable": true}
  },
  "pid":             482711,
  "uptime_s":        14823
}
```

Codes: `ok`, `daemon_down`, `socket_unreachable`.

#### 2.1.2 `egress`

```jsonc
"data": {
  "destinations": [
    {"kind": "ollama",     "url": "http://localhost:11434", "configured_via": "default"},
    {"kind": "otlp",       "url": "http://otel.local:4318", "configured_via": "VESKA_OTLP_ENDPOINT"},
    {"kind": "metrics",    "listen": "127.0.0.1:9090",       "configured_via": "config:metrics.listen"},
    {"kind": "vuln_source","url": "https://api.osv.dev",     "configured_via": "config:vuln_source.provider"}
  ]
}
```

Every configured outbound is enumerated. Unset destinations are
omitted, not represented as `null`. `configured_via` cites the
provenance (default | env var name | `config:<key>`).

Codes: `ok`, `unexpected_egress` (warn — one of the destinations
is enabled in a config layer the operator may not realise).

#### 2.1.3 `storage`

```jsonc
"data": {
  "veska_home":     "/home/jeff/.veska",
  "db_path":         "/home/jeff/.veska/veska.db",
  "db_size_bytes":   1287654321,
  "wal_size_bytes":  8388608,
  "vec0_size_bytes": 251658240,
  "free_bytes":      442891534336,
  "free_ratio":      0.81           // free / (free + db + wal + vec0)
}
```

Codes: `ok`, `disk_low` (free_bytes < 1 GiB), `disk_full`
(free_bytes < 100 MiB; broken).

#### 2.1.4 `embedder`

```jsonc
"data": {
  "endpoint":          "http://localhost:11434",
  "ollama_reachable":  true,
  "model":             "nomic-embed-text",
  "model_present":     true,
  "probe_latency_ms":  127,
  "remediation":       null      // string with copy-pasteable command on degraded
}
```

Codes: `ok`, `model_missing`, `ollama_unreachable`. On
non-`ok`, `remediation` carries the platform-specific install or
`ollama pull` command (cross-ref SOLO-03 §3.2).

#### 2.1.5 `config`

```jsonc
"data": {
  "effective": { /* merged config — full struct, secrets redacted */ },
  "sources": [
    {"key": "metrics.listen", "value": "127.0.0.1:9090", "from": "env:VESKA_METRICS_LISTEN"},
    {"key": "embedder.model", "value": "nomic-embed-text", "from": "default"}
  ]
}
```

Secret-shaped values (any key matching `*_token`, `*_secret`,
`*_password`, `*_key`, `api_key`, `auth*`) are replaced with the
literal string `"<redacted>"` in both `effective` and `sources`.
The redaction rule is shared with `bundle` (§2.2).

Codes: `ok`, `config_invalid` (broken).

#### 2.1.6 `post_promotion_queue`

```jsonc
"data": {
  "by_state_and_kind": {
    "embed":      {"pending": 142, "running": 1,  "done": 9821, "failed": 0},
    "auto_link":  {"pending": 3,   "running": 0,  "done": 612,  "failed": 0},
    "revalidate": {"pending": 0,   "running": 0,  "done": 84,   "failed": 0},
    "review":     {"pending": 0,   "running": 0,  "done": 12,   "failed": 1}
  },
  "queue_depth":      146,
  "high_water":       1000,
  "low_water":        100,
  "failed_rows":      [             // every `failed` row, all kinds
    {"seq": 4711, "kind": "review", "repo_id": "...", "branch": "main", "attempts": 4, "error": "..."}
  ]
}
```

Codes: `ok`, `queue_high` (queue_depth > high_water),
`failed_rows_present` (any `failed` row outside `review`; user
should investigate and retry — degraded, not broken),
`post_promotion_queue_invariant_broken` (a `review` failed row has no
companion `review-pipeline-failure` finding; broken).

#### 2.1.6a `pipelines`

```jsonc
"data": {
  "writer_pools": {
    "hot":   {"in_use": 0, "wait_count": 12, "wait_duration_ms": 340, "busy_timeout_ms": 5000},
    "embed": {"in_use": 1, "wait_count": 0,  "wait_duration_ms": 0,   "busy_timeout_ms": 30000}
  },
  "resync": {
    "state":         "idle",                  // "idle" | "running" | "diverged"
    "repos_pending": 0,                       // repos still to replay
    "current": null                            // {repo_id, from_sha, to_sha, commits_total, commits_done} when running
  },
  "review": {
    "enabled":             true,
    "specialties_active":  ["security"],
    "tokens_today":        128400,
    "tokens_this_commit":  0,
    "caps": {
      "max_tokens_per_commit": 100000,
      "max_tokens_per_day":    500000
      // (USD caps come with hosted LLM providers; not present today.)
    },
    "halted_until":        null            // RFC 3339 reset time when a cap is tripped
  }
}
```

Codes: `ok`, `writer_pool_saturated` (any pool's
`wait_duration_ms` over the most recent window exceeds budget),
`resync_running` (degraded), `resync_diverged` (broken),
`review_paused_token_cap`, `review_paused_usd_cap`. The
cap-paused codes are degraded
(exit 1), not broken — the daemon is functioning as designed,
just declining new review work until the next reset.

#### 2.1.7 `service`

```jsonc
"data": {
  "supervisor":       "systemd",                 // "systemd"|"launchd"|"none"
  "registered":       true,
  "loaded":           true,
  "running":          true,
  "restarts_1h":      0,
  "broken_marker":    null                       // path string when crash-loop tripped
}
```

Codes: `ok`, `not_running`, `flapping` (restarts_1h ≥ 2),
`crash_loop_tripped` (broken; `veska doctor reset-crash-loop`
is the remediation).

#### 2.1.8 `backup`

```jsonc
"data": {
  "latest_path":          "/home/jeff/.veska/backups/2026-05-08T03-00-12.tar.gz",
  "age_seconds":          129831,
  "size_bytes":           1294857832,
  "tarball_opens":        true,
  "integrity_check":      "ok",                  // "ok"|"failed"|"skipped"
  "staleness_warn_s":     604800,
  "required":             false
}
```

Codes: `ok`, `stale` (age > staleness_warn AND not required),
`backup_required_missing`, `backup_corrupt`.

#### 2.1.9 `identity`

Per SOLO-10 §7. Schema:

```jsonc
"data": {
  "process_owner":   "jeff",
  "cli_actor_id":    "human:jeff",
  "mcp_clients": [
    {"actor_id": "agent:claude-code", "actor_kind": "agent", "connected_since": "2026-05-09T17:01:11Z"}
  ],
  "audit_log_path":  "/home/jeff/.veska/audit.jsonl",
  "audit_log_size":  4218304,
  "refused_writes_24h": [
    {"ts": "2026-05-09T18:30:02Z", "gate": "close.finding.high", "actor_id": "agent:claude-code", "reason": "requires actor_kind=human"}
  ]
}
```

Codes: `ok`, `anon_actor_connected` (warn — a connected MCP
client did not declare a name).

#### 2.1.10 `reset-crash-loop`

```jsonc
"data": {
  "broken_marker_existed": true,
  "broken_marker_path":    "/home/jeff/.veska/state/broken",
  "removed":               true
}
```

Codes: `ok`, `no_marker` (informational — there was nothing to
reset).

### 2.2 Diagnostic bundle

`veska bundle` writes a single tarball — meant for
attaching to a GitHub issue or sharing with another developer —
that contains every operator-visible diagnostic the daemon can
produce, with secrets and user content redacted. The command is
**always user-initiated**; nothing leaves the machine
automatically. This is the only way the daemon supports remote
debugging without violating the "zero telemetry by default"
contract (PRODUCT.md §"Privacy & telemetry").

```
veska bundle [-o <path>]
```

Defaults to `./veska-doctor-bundle-<ts>.tar.gz`. Honors `-o
<path>` for an explicit destination. Exits 0 on a successful
write; 2 on a write failure (disk full, permission denied).

**Bundle contents:**

```
veska-doctor-bundle-2026-05-09T18-42-31Z.tar.gz
├── manifest.json                  # what's in the bundle, redaction summary
├── doctor/
│   ├── summary.json               # `veska doctor --json`
│   ├── status.json                # `veska doctor status --json`
│   ├── egress.json                # `veska doctor egress --json`
│   ├── storage.json
│   ├── embedder.json
│   ├── config.json                # secrets redacted (§2.1.5 rule)
│   ├── post_promotion_queue.json
│   ├── pipelines.json
│   ├── service.json
│   ├── backup.json
│   └── identity.json
├── logs/
│   ├── stderr-tail.log            # last 50,000 lines of daemon stderr (full retained-rotation set if smaller)
│   └── error-ring.log             # last 500 warn/error/fatal slog records, regardless of age
├── audit/
│   └── audit.jsonl.tail           # last 5,000 lines of audit.jsonl, redacted (see below)
└── env/
    ├── go-version.txt             # `go version` of the daemon binary
    ├── sqlite-version.txt         # SQLite + sqlite-vec versions
    ├── ollama-version.txt         # `ollama --version` if reachable
    └── platform.txt               # uname -a / sw_vers
```

**Bundle exclusions** (never included, no flag overrides):

- `veska.db` and `veska.db-wal` — schema, vector bytes, raw
  node content. Too large; contains source code.
- Full `audit.jsonl` history (rotations) — only the live tail.
- Repo contents from any registered `Repo`.
- Any embedding tensor or LLM prompt/response payload.
- The user's home directory beyond `~/.veska/` resolved paths.

**Redaction rules** (applied in addition to exclusions):

- **Key-name match** on `config.json`: any field whose key
  matches `*_token`, `*_secret`, `*_password`, `*_key`,
  `api_key`, or `auth*` (case-insensitive) has its value
  replaced with `"<redacted>"`. Same rule as §2.1.5.
- **Key-name match** on `audit.jsonl` lines: the `args` field
  is walked recursively; the same key pattern triggers the same
  replacement.
- **Path scrubbing**: file paths under `$HOME` are emitted
  relative to a placeholder `~/`, not the absolute path.

**Content-shaped scan (in addition to key-name match).** Every
string value walked by the redactor (audit `args`, `args.reason`,
`message`, `title`, free-form fields) runs through the
SecretsScanner port's regex+entropy ruleset (the same one the
promotion pipeline uses, SOLO-11 §2.1). Matches are replaced with
`"<redacted:<rule>>"` (e.g. `<redacted:aws-access-key>`) so the
operator can see *what* was redacted without seeing the value.
This catches the long-tail case where a secret lands in a
non-secret-named field — for a tool whose product surface
*finds* secret leaks, ironically letting them flow through the
audit log was a design footgun.

**What the content scan does NOT catch.** The scanner is
finite-rule; novel secret shapes (custom token formats, generic
high-entropy strings under the entropy threshold) still pass
through. The bundle ships as **operator data, not sanitised
data** — open it before sending. Source content is excluded
entirely (node bodies, embedding bytes, LLM payloads are never
in the bundle).

Treat the bundle as **operator data, not sanitised data**. Open
it before sending; remove anything sensitive that slipped past
the pattern rules.

`manifest.json` carries the bundle schema version (the same
`schema_version` integer as §2.1), the timestamp, the
remediation context (free-form string the user supplies via
`-m "..."` if any), and a count of redactions applied per file.

The tarball is plain `tar.gz`. No encryption. No signing. The
user owns where it goes from there.

### 2.3 Filesystem allowlist (the `fs` section of `veska doctor --json`)

`~/.veska/` houses a SQLite database in WAL mode plus its
`-shm` shared-memory segment. Both have hard requirements about
the underlying filesystem's locking and mmap semantics. The
allowlist below is normative; `veska doctor fs` checks the
filesystem under `$VESKA_HOME` and exits with code 0/1/2 per
the §2 contract.

| Filesystem | Status | Notes |
|---|---|---|
| **ext4, xfs, btrfs (no COW reflinks on the data dir)** on Linux | supported | The default on every mainstream distribution. |
| **APFS** on macOS | supported | The default. SQLite WAL+SHM works correctly. Time Machine clones of `~/.veska/` while the daemon is running are unsafe (see SOLO-08 §9 — use `veska backup create` instead). |
| **HFS+** on macOS | supported | Legacy; works. |
| **tmpfs** | supported but discouraged | Loses durability across reboot. Useful for tests; not for a real `~/.veska/`. The daemon does not refuse to run; `veska doctor fs` warns. |
| **btrfs with `nodatacow` disabled (default)** | supported with a caveat | Copy-on-write of the SQLite file is correct but inflates write amplification under WAL churn. The daemon does not refuse; `veska doctor fs` warns once. Operators with large graphs should `chattr +C ~/.veska` on a fresh dir. |
| **zfs** on Linux/FreeBSD | supported | Same caveats as btrfs CoW; same warning. |
| **NFS / SMB / network filesystems** | refused | SQLite WAL requires the same process to hold both the `-wal` and `-shm` mappings; networked filesystems break this. The daemon refuses to start with `ErrUnsupportedFilesystem`. |
| **eCryptfs** | refused | Inflicts mmap inconsistency on the SHM segment. The daemon refuses to start. Move `$VESKA_HOME` to a non-eCryptfs location. (Note: this affects some Ubuntu installs that still use eCryptfs for `$HOME`; the user picks `~/.veska` outside `$HOME` via `VESKA_HOME`.) |
| **overlayfs (devcontainers, Docker bind-mount edge cases)** | refused if `~/.veska/` lands on the upper layer of an overlay; supported on a tmpfs / volume mount | Devcontainers commonly mount the project root as overlayfs. Veska's data must live on a path that is *not* overlay (a named volume, a tmpfs, or a bind-mount). `veska doctor fs` detects overlay and refuses with a remediation pointing at named-volume setup. |
| **FUSE filesystems generally (sshfs, gcsfuse, s3fs)** | refused | Same WAL/SHM issue as NFS. |

The check uses `statfs(2)` on Linux (`f_type` field) and
`statfs(2)` on macOS (`f_fstypename`). When the filesystem is in
the **refused** column, `veska doctor fs` exits 2 and the
daemon refuses to start with the same error. The remediation
text names a supported filesystem and the `VESKA_HOME`
override.

This list is not exhaustive — exotic filesystems (gfs2, ocfs2,
9p) are not in the allowlist; the doctor reports them as
**unknown** with exit 1 and recommends moving the data dir to a
known-good filesystem rather than guessing.

## 3. Performance budgets

Reference hardware: 8-core, 16 GiB RAM, NVMe SSD. M1 MacBook Air
or equivalent x86-64 laptop.

**Number labels.** Every number in this section carries one of:

- `BUDGET (unmeasured)` — a target picked from informed estimate.
  Not a decision. Replaced with a measurement at the named gate.
- `BUDGET (measured M<N>)` — measured on the reference laptop;
  cites the spike commit hash.
- `INVARIANT` — a fact (file dim, schema PK, protocol shape).
- `DEFAULT` — a knob with a sensible-but-arbitrary value;
  override in config (CONFIG-SURFACE.md).

A `BUDGET (unmeasured)` that misses at its gate gets rewritten
or replaced by an ADR. Budgets do not silently move.

The labelling binds the design tree (`design/SOLO-*`).
Milestone docs and the open-questions register may restate a
budget inline; on divergence, this section wins.

### 3.1 Hot path (interactive, daemon-global)

| Operation | Budget | Label | Gate |
|---|---|---|---|
| `find_symbol` warm p95 | < 50ms | BUDGET (measured M1): 0.072ms at 50k nodes, bench commit c6388b9 (PASS) | M1 |
| `find_symbol` cold p95 (first call after start) | < 250ms | unmeasured | M1 |
| `get_node` p95 | < 25ms | BUDGET (measured M0): 0.039ms at 100k nodes, spike 72d6ca4 (PASS) | M1 |
| `get_edges` p95 | < 100ms | BUDGET (measured M0): 0.047ms at 100k nodes, spike 72d6ca4 (PASS) | M1 |
| `get_call_chain` p95, depth 3, single repo | < 100ms | unmeasured | M1 |
| `semantic_search` p95 (k=10, 50k vectors) | < 100ms | BUDGET (measured M1): 1.90ms via application-layer UsearchStore at 50k vectors, recall@10=0.987, bench commit in m1.hnsw-pivot (PASS); OQ-S001 resolved — integrated path confirmed << M0 vec0 ceiling | M1 ✓ |
| Save → staging visible to MCP (post-debounce; reparse + staging update), **typical file** (≤ `[save].large_file_threshold_loc`, DEFAULT 1500 LOC) | < 50ms | unmeasured | M1 |
| Save → staging visible to MCP, **large file** (> threshold) | reparse runs in background; **staging serves the prior good entry for that file** and MCP reads stamp `degraded_reasons: ["staging_reparsing:<path>"]` until the reparse completes; budget < 500ms p95 for the badge-clear, not the save→staging-visible path | unmeasured | M1 |
| Post-commit hook return p95, **typical commit** (see "refactor-commit definition" below), Linux | < 100ms | BUDGET (measured M1): 0.116ms p95 (500 iterations, Unix socket round-trip), bench commit in m1.04 (PASS) | M1 ✓ |
| Post-commit hook return p95, **typical commit**, macOS | < 200ms (uniform `synchronous = FULL` per §3.1a; `F_FULLFSYNC` is 5–50ms vs Linux 1–10ms) | unmeasured | M1 |
| Post-commit hook return p95, **refactor commit** | < 5s | BUDGET (measured M1): 3.83s p95 for 50k-node promotion tx (20 trials), bench commit 662a951 (PASS) | M1 ✓ |
| Promotion barrier wait (raised → `BeginTx` acquires `writeDB.hot`) | ≤ 50ms p95; counts toward the typical/refactor row above, not in addition | unmeasured | M1 |

**Refactor-commit definition.** A "refactor commit" promotes more
than `[budget].refactor_commit_threshold_symbols` (DEFAULT 5000)
distinct symbols from staging or replays a `git log` window of
the same magnitude. Below the threshold the typical row applies.
The split exists because synchronous structural checks (SOLO-11
§2.1) scale roughly linearly with promoted symbol count; a large
rename that touches 300 callers but produces few new symbols
falls under typical. M1 measurement reports the curve so the
threshold can be sharpened to a measured value.

**The promotion barrier is what makes the typical-commit budget
hold under agent write bursts.** Once `Promote` arrives at the
daemon, new MCP write entrants block on the barrier until the
promotion commits, so the promotion's wait at `writeDB.hot` is bounded by
at most one in-flight MCP write rather than by the agent's burst
length (SOLO-11 §10.2; ADR-S0011). The hook-return rows reflect
the promote-vs-checks split (SOLO-11 §2): synchronous structural
checks scale with commit size, so typical and refactor commits
have separate budgets. If the M1 writer-contention bench (§3.4)
shows hot-pool wait exceeding the budget despite the barrier,
the fix path is bench-driven (e.g., shrinking embed chunk size)
rather than a priority lane.

### 3.1a fsync defaults — uniform FULL

`[storage].synchronous = "FULL"` on every platform. SOLO-08 §5.1
records the SQLite-level mechanics; this section records the
budget consequence.

`F_FULLFSYNC` on macOS honours the disk-cache flush and runs
5–50ms on consumer SSDs (vs. 1–10ms on Linux ext4/btrfs). The
macOS typical-commit row above is therefore < 200ms rather than
< 100ms; the refactor row absorbs the same delta inside its
existing 5s budget.

**Why not split by platform.** The prior version of this section
shipped macOS at NORMAL. The defence was that startup-resync
(SOLO-03 §5.7) replays missing promotions from Git on next
start. That argument depends on Git itself fsync-ing the HEAD
update; Git's `core.fsync` defaults vary by version and distro
and don't reliably cover ref updates on every install. On a
real power-cut both the Veska promotion and the Git HEAD
advance can be lost independently, after which
`last_promoted_sha == HEAD` looks consistent and resync sees no
work — but the user's commit is gone. We eat the budget cost
for honest durability rather than ship a defence that doesn't
fully hold.

Operators who want the lower-durability NORMAL mode (e.g. for
short-lived CI worktrees) set `[storage].synchronous = "NORMAL"`
in config and accept the trade.

M1 measurement validates the macOS row rather than discovers it.

### 3.1b Large-file save fallback

`[save].large_file_threshold_loc` (DEFAULT 1500) gates whether a
save→staging reparse runs on the typical-budget path or the
large-file path. fsnotify delivers "this file changed," not edit
deltas, so tree-sitter reparse is whole-file and cost scales
roughly linearly with LOC.

**Behaviour above the threshold:**

1. The save event is debounced normally.
2. The reparse runs in a background goroutine, not on the
   save-handler hot path.
3. Until the reparse commits, MCP reads against that file see
   the **prior good staging entry** (or promoted, if no prior
   staging exists) and stamp
   `degraded_reasons: ["staging_reparsing:<path>"]`.
4. When the reparse commits to staging, the badge clears for
   that file.

This trades save→staging freshness for hot-path responsiveness:
the editor never waits on tree-sitter for a 5k-LOC file; reads
stay current within the prior save's freshness until the reparse
catches up. M1 measures the badge-clear p95 to confirm the
"< 500ms" row above is reachable on real workloads.

Incremental-parse via `ts_parser_parse` with the prior tree is
the obvious next step, but fsnotify does not deliver edit ranges
— the editor would have to push them, which is editor
integration work and out of M1 scope. **Revisit after M3** if
real-workload measurements show the threshold is too tight; do
not pre-author an incremental-parse ADR before the data exists.

### 3.2 Background / batch

| Operation | Budget | Label | Gate |
|---|---|---|---|
| Cold-scan 100k LOC repo (per repo) | < 60s wall-clock | BUDGET (measured M1): 1.616s total (1000 files ~119k LOC, GoParser), bench commit in m1.05 (PASS) | M1 ✓ |
| Promotion SQL transaction (atomic write portion only) | < 1s p95 typical, < 5s p95 refactor | BUDGET (measured M1): 3.83s p95 refactor (50k nodes + edges, WAL, 20 trials), bench commit 662a951 (PASS); typical-commit split not yet isolated | M1 ✓ (refactor gate) |
| In-tx synchronous review checks (dead-code + secrets + vuln + contract drift) | budget *included* in the §3.1 hook-return row, not additive | unmeasured | M1 |
| Embedding throughput on CPU Ollama | depends heavily on model + CPU + concurrent load — varies from seconds (small commit, idle laptop, quantised model) to hours (refactor commit, busy laptop, full nomic-embed-text); refactor-commit p95 unmeasured | unmeasured | M3 measures, publishes a matrix (model × CPU class × commit size) rather than picking one number |
| Auto-link drain after 10k-edge commit | < 60s | unmeasured | M3 |
| Cold daemon startup (warm sqlite-vec cache) | < 2s | unmeasured | M1 |
| Wake-reconcile sweep on suspend/wake (per repo, working tree only) | < 500ms p95 typical repo; < 5s for working trees with > 50k tracked files | unmeasured | M1 |
| Wake-reconcile total (N registered repos, **parallel** across repos, capped at `[watcher].wake_concurrency` = #cores / 2) | dominated by the slowest repo's sweep, not the sum; MCP reads against promoted state serve concurrently from `readDB` from t=0 with `degraded_reasons: ['wake_reconciling']` | unmeasured | M1 |

The **promotion-budget stacking note** is load-bearing. The §3.1 row
"Post-commit hook return p95, refactor commit < 5s" is the
*total* wall-clock budget the user sees: it covers the promotion SQL
transaction, the in-transaction structural checks (dead-code,
secrets-scan, vuln-scan, contract-drift; SOLO-11 §2.1), and any
SQLite writer-lock contention the promotion absorbs. The promotion SQL row
above is the SQL-only portion; the in-tx checks share the same
5s budget, not a parallel one. M1 measurement reports the split
so we know which sub-step dominates a refactor commit.

The **save→staging large-file row** acknowledges that fsnotify
delivers "this file changed," not edit deltas, so tree-sitter
reparse is whole-file. A multi-thousand-line TS/TSX file on a
cold parser cache will not hit the 50ms typical-file budget. M1
measures both the actual cold-reparse curve by file size and the
threshold above which the typical budget breaks. **Fallback path
if M1 shows the threshold is uncomfortably low:** rank reparse
tasks by file size with the `staging_reparsing:<path>` badge
visible to MCP reads (already specified in SOLO-11 §1), so the
editor surfaces "this file's staging is stale" rather than
silently blocking the save→staging path. No incremental-parse
plan today — the design accepts whole-file reparse and pushes
the cost into a per-file degraded badge.

The **wake-reconcile budget** captures the cost of resyncing the
working tree after macOS FSEvents / Linux inotify drop events
across a suspend (SOLO-03 §5.2). With multiple registered repos
this is the user's first impression on opening the laptop —
make the budget visible so M1 measurement catches it if it stretches.

### 3.3 Resource ceilings (daemon-global)

| Resource | Soft cap | Hard cap | Label |
|---|---|---|---|
| Daemon RSS at steady state, 1 repo | 2 GiB | 4 GiB (kill if exceeded) | BUDGET (measured M1): 17 MiB RSS after 50-branch × 5k-node load (250k rows), bench commit 662a951 (PASS) — SQLite page cache dominates, not row count |
| Daemon RSS at steady state, N repos (working set) | 2 GiB global | 4 GiB global | BUDGET (measured M1): per-repo additive curve is ~17 MiB / repo at 250k rows; 50-repo projection ≈ 850 MiB — well within 2 GiB soft cap; `veska repo add` RSS check uses `internal/repo/rss.go` (commit in m1.10) |
| Goroutines | 200 | 500 (refuse new work above) | DEFAULT |
| File descriptors | 1024 | OS limit | INVARIANT (OS) |
| `~/.veska/` on-disk size | unbounded; surfaced in `veska doctor storage` | — | INVARIANT |
| `audit.jsonl` per-file size | 100 MiB before rotation | 5 rotations kept | DEFAULT |
| Branch-in-PK on-disk size (28 branches × 100k nodes) | ~1.68 GiB measured; ~3.0 GiB extrapolated to 50 branches | — | BUDGET (measured M1): OQ-S006 re-verified — 50 branches × 5k nodes; query p95 0.030ms (M0: 0.040ms, ratio 0.75×, GREEN); no ≥2x regression; bench commit 662a951. M0 curve confirmed. |

The 4 GiB hard cap is global, not per-repo. `veska repo add`
refuses to register a new repo if its estimated steady-state cost
would push the projection past the soft cap (SOLO-03 §3.0). The
projection function `f(repo_size_kloc) → bytes` lands at M1 close
once the 1-repo and N-repo measurements are real numbers; until
then, `veska repo add` uses a conservative linear estimate
(working number: 20 MiB per 10 kLOC indexed) and reports it
explicitly to the user before committing.

#### 3.3.1 The vec0 scale ceiling — explicit gate

The 4 GiB cap interacts with the vec0 substrate (ADR-S0001) in a
way the design must name out loud rather than discover at M1.
Brute-force vec0 holds the entire vector population in memory at
query time. The arithmetic for the relevant breakpoints, on the
default 768-dim `nomic-embed-text` configuration:

| Population | Vector bytes (768-dim f32, ~3 KB each) | + 3-pool mmap (768 MiB) | + working set (~500 MiB) | RSS estimate |
|---|---|---|---|---|
| 100 000 nodes | ~300 MiB | ~1.07 GiB | ~1.55 GiB | comfortably under soft cap |
| 250 000 nodes (the proposed `[ceiling].minimum_for_m1` floor) | ~750 MiB | ~1.5 GiB | ~2.0 GiB | **brushes the 2 GiB soft cap** before any branch-PK row duplication |
| 500 000 nodes | ~1.5 GiB | ~2.25 GiB | ~2.75 GiB | over soft cap; under hard cap |
| 1 000 000 nodes | ~3 GiB | ~3.75 GiB | ~4.25 GiB | **over the 4 GiB hard cap** |

The 250k floor was picked because it clears typical solo working
sets, but the table above shows it lands at the soft cap on a
single-branch repo with no headroom for branch-PK row growth. A
multi-branch user (50 branches, ADR-S0013 not yet tripped) hits
the soft cap well before 250k vector population because `nodes`
and `edges` row growth eats the same RSS budget that vec0 wants.

Two consequences carried through the rest of the design:

1. The `[ceiling].minimum_for_m1` floor at 250 000 nodes is for
   a **single-branch working set**. Multi-branch repos hit the
   ceiling earlier in proportion to their branch overlap; the
   doctor surface (below) reads the live RSS curve, not the
   vec0 population alone.
2. The OQ-S006 / ADR-S0013 trip directly affects this ceiling.
   On the flat-PK shape, every branch's nodes/edges sit in the
   same RSS budget that vec0 wants; on the trunk+delta shape
   (ADR-S0013 accepted) the row tables shrink and the vec0
   population gets back its share.

This is arithmetic, not a future risk. M0 is a **hard go/no-go
gate** for M1's substrate, not just a measurement exercise:

- **M0 measures the vec0 ceiling.** Epic m0.01 records the node
  count at which the brute-force path crosses (a) the
  `semantic_search` p95 budget in §3.1, or (b) the soft RSS cap
  (2 GiB) at steady state. Whichever comes first is the
  **measured vec0 ceiling**.
- **The minimum-ceiling floor.** `[ceiling].minimum_for_m1`
  (DEFAULT 250 000 nodes; CONFIG-SURFACE) is the floor below
  which vec0 is not a shippable substrate. The number matches
  M0's "Red — ceiling" outcome (milestones/M0.md): a measured
  ceiling below 250 000 nodes promotes the HNSW pivot ADR
  (OQ-S003) from M3 work into M1 work, mandatory before
  m1.03 begins.
- **Formal go/no-go at m0.01 close:**
  - **Green** (measured ceiling ≥ `[ceiling].minimum_for_m1`):
    M1 ships vec0. The §3.1 `semantic_search` p95 row relabels
    `BUDGET (measured M0)`; the runtime `vec0_ceiling_warn` /
    `vec0_ceiling_exceeded` thresholds (see "Doctor surface"
    below) are the per-user guard.
  - **Red** (measured ceiling < `[ceiling].minimum_for_m1`):
    M1 **does not ship vec0**. The M2 HNSW pivot ADR (OQ-S003)
    is brought forward into M1 scope and M1 ships only when
    HNSW is ready. M1 does not ship a substrate the doc says
    cannot serve typical workloads.
  - There is no yellow band: a measurement landing within
    ±10% of the floor lands red. The decision is recorded in
    m0.01's close report as `gate: green | red`.
  - **BUDGET (measured M0): gate = RED.** Spike 4d63d34 measured
    the vec0 ceiling at **100 000 nodes** (p95 latency budget
    crossed before the RSS soft cap). This is below the 250 000
    node floor. ADR-S0014 mandates the HNSW pivot before m1.03.
    M1 **does not ship vec0**.
- **Runtime guard regardless of gate outcome.** Even on green,
  M1 ships the doctor surface below. A green M0 measurement
  does not mean every user's working set fits; the runtime
  guard catches the long-tail user whose repo exceeds the
  measured ceiling.
- **HNSW landing site.** On green, HNSW is M2 work (OQ-S003
  pivot ADR written at M2 entry against M1's curves). On red,
  HNSW is M1 work and M1's exit gate slides until it lands.
  Either way the ADR specifies the backing (lancedb embedded
  vs. hnswlib via cgo), the recall floor against a fixed eval
  set, and how the choice preserves the SOLO-08 §9
  single-tarball backup property (or documents the regression).

The 250 000 threshold cited in PRODUCT.md "What it costs" is a
working number for the user-facing copy; m0.01's report either
confirms it or names the actual measured ceiling with the curve
to back it. The substrate decision no longer precedes the
measurement that justifies it.

**Doctor surface for the M1 ship.** When M1 ships with the
vec0 substrate, the daemon must make the ceiling visible
*before* the user crosses it, not after. `veska doctor
storage --json` carries an `embeddings` block:

```json
"embeddings": {
  "substrate":          "vec0",          // "vec0" | "hnsw" once M2 lands
  "node_count":         182334,
  "ceiling":            250000,           // m0.01's measured ceiling
  "headroom_ratio":     0.27,             // (ceiling - count) / ceiling
  "vec0_bytes":         559091712,
  "status":             "warn",          // "ok" | "warn" | "exceeded"
  "advice":             "approaching vec0 ceiling; M2 ships HNSW substrate"
}
```

Status thresholds (DEFAULT, CONFIG-SURFACE):

- `ok` — `headroom_ratio ≥ 0.25`.
- `warn` — `0.05 ≤ headroom_ratio < 0.25`. The warning fires
  in `veska doctor` digest output and `veska_doctor_status`
  metric labels (`status="warn"`); MCP reads against
  `semantic_search` add `degraded_reasons:
  ["vec0_ceiling_warn"]`.
- `exceeded` — `headroom_ratio < 0.05`. `semantic_search` p95
  budget is presumed missed; reads stamp `degraded_reasons:
  ["vec0_ceiling_exceeded"]`. The daemon does *not* refuse
  reads; it serves what vec0 can serve and tells the truth.

This is the M1 contract that closes the "ship a substrate the
doc itself says cannot serve the workload" gap: M1 ships vec0,
the user sees the headroom in `veska doctor` from day one, and
M2's HNSW pivot has a documented landing site (the same
`embeddings.substrate` field flips to `"hnsw"`). The PRODUCT.md
"What it costs" section names this contract for buyers.

### 3.4 Multi-repo budgets

Multi-repo composition (SOLO-03 §3.0) introduces budgets for
queries that fan out across repos and for cross-repo edge
resolution (SOLO-11 §9).

| Operation | Budget | Label | Gate |
|---|---|---|---|
| `get_call_chain` p95, depth 3, `repo: "*"`, ≤ 5 indexed repos, ≤ 1 cross-repo hop | < 250ms (includes audit-append on every cross-repo-touching read per SOLO-10 §4) | unmeasured | OQ-S010 (M1) |
| `get_blast_radius` p95, single src node, `repo: "*"`, one-hop | < 250ms (includes audit-append) | unmeasured | OQ-S010 (M1) |
| `semantic_search` p95 (k=10, 50k vectors) `repo: "*"` | < 150ms | unmeasured | M1 |
| Cross-repo resolver lookup (one indexed point query) | < 5ms p95 | BUDGET (measured M1): 339µs p95 (2 repos × 1000 nodes, 200 stubs, 1000 runs), bench commit in m1.10 (PASS) — OQ-S010 RESOLVED GREEN | OQ-S010 ✓ |
| Audit-append cost on a cross-repo read (synchronous, per SOLO-10 §4) | < 2ms p95 (one O_APPEND fsync + JSON marshal); included in the row above, not additive | unmeasured | M1 |
| Cold-scan total wall-clock for N repos at `repo add` time | linear in repo size; concurrent across repos | DEFAULT (concurrency = #cores / 2) | M1 confirms |

The first four rows are gated by **OQ-S010** (open-questions §10).
The M1 bench (sub-issue `m1.10.9-bench`) measures all four; the
OQ's green/yellow/red outcomes determine whether the SOLO-11 §9.1
materialised-cache fallback ships as an ADR. SOLO-04 §5.4's
same-Graph invariant is preserved whether or not the cache lands.

### 3.5 Wiki and context-pack

| Operation | Budget | Label | Gate |
|---|---|---|---|
| `eng_get_context_pack` p95 (typical task) | < 200ms warm | unmeasured | M1 |
| `hot_zone` regeneration on 100k-LOC repo | < 2s | unmeasured | M1 |
| `entry_points` regeneration on 100k-LOC repo | < 1s | unmeasured | M1 |

Budgets here are pure-function-of-promoted-state regenerations
(SOLO-12); no LLM is in the path. If a budget misses we rewrite
the row, not the design.

### 3.6 Pipelines and review

| Operation | Budget | Label | Gate |
|---|---|---|---|
| Save → staging update, **fsnotify path** (post-debounce; tree-sitter reparse + staging write — debounce window itself is a coalescing knob, not in the budget) | "imperceptible"; concrete number unmeasured | unmeasured | M1 |
| Save → staging update, **polling-fallback path** (FSEvents budget exceeded; SOLO-03 §3.0 names the trigger) | bounded by `[watcher].poll_fallback_interval` (DEFAULT 5s) — this is a regime change, not a degraded fsnotify run; user perception is "edits show up after the next poll tick" | DEFAULT (regime) | M1 documents the engagement criteria |
| Revalidation sweep at 1k / 10k / 100k open findings | "well under the cadence interval" | unmeasured | M2 |
| Review pipeline per-commit (security specialty), local Ollama | seconds-to-minutes per touched file; range unmeasured | unmeasured | M5 measures, picks a number |
| Review pipeline per-commit (contract specialty), local Ollama | seconds per touched file; faster than security | unmeasured | M5 |

The review pipeline's hard-halt ceilings (per-commit tokens, daily
tokens; USD/weekly caps arrive with hosted LLM providers) are not perf budgets — they are operator
cost controls. The defaults and behavior are normative in
SOLO-11 §3 and CONFIG-SURFACE.md `[review]`; the degraded-mode
row in §4 covers what the system does when one trips.

Where prose elsewhere in the design tree (SOLO-11, SOLO-12,
PRODUCT, ADR-S0001) cites a number, that prose either cites this
section or the number is wrong. SOLO-13 §3 is the canonical home.

## 4. Degraded modes

Veska returns partial answers rather than refusing service when a
dependency is unavailable. MCP responses carry
`degraded_reasons: [...]` so callers know the answer is partial.

**Grammar (normative).** Each entry is an object, not a string:

```json
{
  "code": "embedding_pending",            // snake_case, required, stable vocabulary
  "details": {                            // object, optional, schema per code
    "node_id": "n_...",
    "age_seconds": 142
  }
}
```

Older shipping clients accepting only string entries SHOULD be
updated to read `code`. The string-with-colon form
(`"post_promotion_queue_deferred:embed:42"`) used in earlier
drafts is replaced by `{"code": "post_promotion_queue_deferred",
"details": {"work_kind": "embed", "count": 42}}`. The full
`code` vocabulary is enumerated in SOLO-09 §4.5.

| Failure | Behavior |
|---|---|
| Ollama down or unreachable | `semantic_search` falls back to FTS5 BM25 over `node_fts` (SOLO-08 §3.3) with `degraded_reasons: ['embedder_unreachable', 'embedder_offline_lexical_fallback']`. Lexical recall ranks worse than vector recall on synonym/paraphrase queries; the agent sees the reason and should not silently switch reasoning modes. New embeddings stay `pending` until Ollama returns. Promoted structural queries unaffected. |
| post-promotion queue at high-water with embedder paused (formerly a documented deadlock) | Promotion proceeds; new `embed` rows insert with `state='deferred'` instead of blocking the promotion. `degraded_reasons: ['post_promotion_queue_deferred:embed:<count>']` until depth drops below low-water. (SOLO-08 §3.4.) |
| Ollama model missing | Daemon refuses to start the embedder worker; `veska doctor embedder` reports the gap. Other tools function. |
| Disk full | Promotion fails with a clear error; hook returns non-zero; daemon refuses new MCP writes (`degraded_reasons: ['disk_full']`) until `veska doctor storage` reports OK. |
| sqlite-vec extension missing | **Daemon refuses to start.** Loud failure with install instructions. We do not silently degrade core search. |
| SQLite database locked by an external writer | `BEGIN IMMEDIATE` blocks until `busy_timeout` expires (5s hot, 30s embed); op then fails. In-process pools (`writeDB.hot`, `writeDB.embed`) serialize via SQLite's lock without contending — this row fires only when the user opens an external `sqlite3` connection or similar (SOLO-11 §10, ADR-S0011). |
| MCP write tool's `max_wait_ms` deadline expires waiting for `writeDB.hot` | Tool returns `ErrBusy` with `data.context.cause = "seal_in_flight"` (carrying `promotion_id`, `eta_ms`) or `"pool_wait"` (carrying `wait_count`, `wait_duration_ms`, `eta_ms` from `sql.DBStats()`). Surface in `veska doctor pipelines` as `writer_pool_saturated`. (SOLO-09 §4.6.) |
| Memory pressure (RSS > 2 GiB soft cap) | Goroutine count caps; embedder worker pauses; tree-sitter reparse coalesces. **MCP does not refuse requests.** Surface in `veska doctor`. |
| RSS > 4 GiB hard cap | Daemon logs and exits non-78; supervisor restarts; counts against the crash-loop breaker (SOLO-03 §5.6). After 5 such exits in 10 min the next start hits SOLO-03 §5.8 row 1 (exit 78, supervisor halts); user clears with `veska doctor reset-crash-loop`. |
| Daemon refuses to start (sqlite-vec missing, schema mismatch, ErrEmbedderMismatch, NFS, etc.) | Exit 78 at start; supervisor halts. SOLO-03 §5.8 is the full matrix. None of these increment the breaker. |
| Vuln-source feed timeout | Cached findings remain valid; refresh fails with `degraded_reasons: ['vuln_feed_unreachable']` next surface. |
| LLM generator (review pipeline) timeout | Review job marked failed in `post_promotion_queue`; retries up to 3 with backoff, then a `review-pipeline-failure` finding (severity `high`) is emitted; the post-promotion queue row stays `failed` until that finding closes through the human-action gate (ADR-S0004). Promoted graph unaffected. |
| Review pipeline daily token cap reached | Pause new review jobs; queued rows stay `pending`; write one line to `audit.jsonl` describing the pause (cap, spent, resets_at). Surface in `veska doctor pipelines`. Resume at local-midnight automatically — no Finding, no human-action gate. (SOLO-11 §3.1.) |
| Tree-sitter parse error on a file | File skipped; finding emitted with `source_layer='structural'`, `rule='parse-failure'`. Other files in the promotion proceed. |
| Watcher (fsnotify) overflow | Daemon falls back to polling at 5s for the affected paths; logs `watcher_overflow`; surface in `veska doctor`. |

The principle: graph queries against promoted state always work.
Embedder, LLM, and vuln-feed dependencies degrade gracefully.

## 5. CI gates

The CI surface is intentionally small. Three checks, no custom
analyser pipeline:

| Gate | Tool | Required to merge |
|---|---|---|
| Tests pass with race detector | `go test -race ./...` | yes |
| Lint clean | `golangci-lint run ./...` | yes |
| Layer rule | `tools/lint/layercheck` | yes |

### 5.1 The single custom analyser

`layercheck` enforces the one rule that matters: **`internal/core/`
imports nothing from `internal/application/` or
`internal/infrastructure/`**. That rule alone preserves the
hexagonal boundary. Everything else is a code-review concern.

We do **not** run: `domainentitynew`, `wireclean`,
`compositeidentity`, `slogattrs`, `nofmtprintf`, `auditshape`,
`embedmigration`, `crossdbrefs`, `typedregistry`, `capabilityschema`,
`docver`. If a coding convention matters, it goes in `CLAUDE.md`
and shows up in review.

### 5.2 What `golangci-lint` runs

Standard set: `govet`, `errcheck`, `staticcheck`, `gosimple`,
`ineffassign`, `unused`, `gofmt`, `goimports`. No custom plugins.

### 5.3 Coverage

We track coverage but do not gate on a percentage. Coverage
reports run on every PR; a drop > 5% on a domain package surfaces
as a review comment, not a blocker.

### 5.4 Pre-commit

Local `pre-commit` hook (optional) runs `go fmt ./...`,
`golangci-lint run`, and `go test -short ./...`. CI is the source
of truth; the hook is a courtesy.

## 6. Audit log

The audit log shape, versioning rule, and reader contract live
in **SOLO-08 §3.5** (canonical). The trigger lives in
**SOLO-10 §4**. No `auditshape` lint; the schema and `v` bump
discipline are documented, not enforced. Rotated at 100 MiB.
Forward elsewhere by tailing the file.

## 7. Cross-references

- SOLO-01 §8 — what we measure and what we don't.
- SOLO-08 §3.5 — audit log shape.
- SOLO-08 §6 — storage failure modes.
- SOLO-11 — pipelines (where the promotion-latency budget originates).
- SOLO-14 — milestone exit gates that resolve every `BUDGET (unmeasured)` row.
