---
title: "Configuration Surface"
status: reference
last_reviewed: 2026-05-08
related: [SOLO-13]
verified: true
verified_date: "2026-05-17"
---

# Configuration Surface

> **Loader status (solov2-s5c.6).** `internal/config` now ships a real
> TOML loader (`config.go`): `Load()` resolves
> **defaults < `~/.veska/config.toml` < env vars** and `Validate()`
> enforces the tracing rule. The struct covers `[daemon]`, `[logging]`,
> `[metrics]`, `[tracing]`, `[storage]`, `[watcher]`, `[embedder]`,
> `[post_promotion_queue]`, `[budget]`, `[llm_generator]`, `[review]`.
> **Consumed today (live):** `embedder.rate_per_sec` and
> `post_promotion_queue.poll_interval` are threaded into
> `cmd/veska-daemon/wire.go`; the four env vars
> (`VESKA_OLLAMA_URL`, `VESKA_EMBED_MODEL`, `VESKA_VECTOR_BACKEND`,
> `VESKA_DEBUG`) override their struct fields. **Planned:** every other
> key is decoded and available on `Config` but not yet read by the
> daemon. Sections not in the struct above (`[parser]`, `[branch_pk]`,
> `[save]`, `[vuln_source]`, `[tracker]`, `[mcp]`, `[memory]`,
> `[supervisor]`, `[backup]`, `[writer.*]`, `[reader]`, `[tokens]`,
> `[retention]`, `[autolink]`) remain documented-but-unparsed.

The whole story. One file (`~/.veska/config.toml`) plus a handful
of environment variables. No per-workspace config files, no
identity policy, no replication policy, no mode selectors.

## 1. Files Engram reads

| File | Purpose |
|---|---|
| `~/.veska/config.toml` | Daemon config. Created by `veska init` if absent. |
| `~/.veska/audit.jsonl` | Append-only audit log. Owned by Engram. |
| `<repo>/.veskaignore` | Per-repo ignore patterns. Plain `.gitignore` syntax. |
| `<repo>/.beads/current_task` | Active-task pin if the `bd-cli` tracker integration is on. |

That's it. Backup is `veska backup create` (SOLO-08 §9), not a
`tar` of the live directory — tarring a running SQLite database
captures inconsistent WAL state.

## 2. Environment variables

| Var | Purpose | Default |
|---|---|---|
| `VESKA_HOME` | Daemon data root. | `~/.veska` |
| `VESKA_CONFIG` | Override the config file path. | `$VESKA_HOME/config.toml` |
| `VESKA_LOG_FORMAT` | `text` or `json`. | `text` |
| `VESKA_LOG_LEVEL` | `debug`, `info`, `warn`, `error`. | `info` |
| `VESKA_OTLP_ENDPOINT` | OTLP exporter target. Enables tracing if set. Overrides `tracing.otlp_endpoint`. | unset |
| `VESKA_METRICS_LISTEN` | Prometheus listener address (e.g. `127.0.0.1:9090`). Enables metrics if set. Overrides `metrics.listen`. | unset |

Env vars override file values. CLI flags override env. No hot
reload; restart for changes to take effect.

## 3. `~/.veska/config.toml`

A complete example with defaults. The surface is split: a
**common** set users tune routinely (top of the file) and an
**advanced** set that exists for tuning corner cases and that
most users will never touch (bottom). The split is documentation,
not enforcement — TOML doesn't care, but it lets a user opening
the file see what is and isn't worth their time.

### 3.1 Common

```toml
# ─── daemon ──────────────────────────────────────────────────
[daemon]
cli_socket_path = "~/.veska/cli.sock"      # CLI connections; actor_kind = 'human'
mcp_socket_path = "~/.veska/mcp.sock"      # MCP shim connections; actor_kind = 'agent'
pid_file        = "~/.veska/daemon.pid"
shutdown_grace  = "5s"                       # graceful-stop window

# ─── logging ─────────────────────────────────────────────────
[logging]
format          = "text"                     # "text" | "json"
level           = "info"                     # debug | info | warn | error
file            = "~/.veska/logs/daemon.log"  # rotated internally
rotate_at_bytes = 104857600                  # 100 MiB
keep_rotations  = 5                          # daemon.log.1..5

# ─── metrics (opt-in) ────────────────────────────────────────
[metrics]
enabled         = false
listen          = "127.0.0.1:9090"           # only bound when enabled; VESKA_METRICS_LISTEN overrides

# ─── tracing (opt-in; off by default) ────────────────────────
# Both `enabled = true` AND `otlp_endpoint` (or
# VESKA_OTLP_ENDPOINT env var) MUST be set; setting one without
# the other is a config error caught at startup. Sample ratio
# is set by the operator at opt-in time; there is no default
# that ships traces.
[tracing]
enabled         = false
otlp_endpoint   = ""                         # e.g. "http://localhost:4318" — required when enabled
sample_ratio    = 1.0                        # set at opt-in time; lower for noisy local profiling

# ─── storage ─────────────────────────────────────────────────
[storage]
db_path                = "~/.veska/veska.db"
journal_mode           = "WAL"               # do not change
synchronous            = "FULL"              # "FULL" (default) | "NORMAL". See SOLO-08 §5.1.
wal_autocheckpoint     = 1000                # pages
idle_checkpoint_after  = "5s"
audit_max_size_mb      = 100
audit_keep_files       = 5

# ─── parser ──────────────────────────────────────────────────
[parser]
languages              = ["go", "typescript"]   # Go + TS
max_file_size_kb       = 1024

# ─── watcher ─────────────────────────────────────────────────
[watcher]
debounce               = "200ms"
poll_fallback_interval = "5s"
wake_threshold         = "30s"               # monotonic-clock gap that triggers wake reconcile (SOLO-03 §5.2)
wake_tick              = "5s"                # cadence of the wake-detector tick
wake_concurrency       = 0                   # parallel repos in wake-reconcile sweep; 0 = runtime.NumCPU() / 2
max_paths_per_repo     = 50000               # repo add admission ceiling (SOLO-03 §3.0)
max_paths_total        = 200000              # daemon-global path-watch ceiling

# ─── budgets ─────────────────────────────────────────────────
[budget]
refactor_commit_threshold_symbols = 5000     # promote count above which the §3.1 refactor budget applies (SOLO-13 §3.1)

# ─── branch-PK row growth (OQ-S006 / ADR-S0013) ──────────────
[branch_pk]
max_growth_per_branch_pct = 10               # % of trunk row count per active branch; M0 spike compares against this to trip ADR-S0013

# ─── save large-file fallback ────────────────────────────────
[save]
large_file_threshold_loc = 1500              # files above this reparse on a background goroutine (SOLO-13 §3.1b)

# ─── embedder ────────────────────────────────────────────────
[embedder]
provider               = "ollama"
endpoint               = "http://localhost:11434"
model                  = "nomic-embed-text"
dim                    = 768
rate_per_sec           = 10
batch_size             = 32

# ─── llm generator (review pipeline; off by default) ─────────
# Ships only the local Ollama generator. Hosted providers
# (Anthropic, OpenAI, Gemini, openai_compatible) and the
# environment-variable key resolution they require come behind a
# future ADR (see SOLO-05 §2.5). Setting `provider` to anything
# other than "ollama" causes the daemon to refuse to start.
[llm_generator]
enabled                = false
provider               = "ollama"            # "ollama" only.
endpoint               = "http://localhost:11434"
model                  = "llama3.1:8b"
timeout                = "60s"

# ─── review pipeline ─────────────────────────────────────────
# Hard halts. See SOLO-11 §3.1 for behavior:
#   - per_commit overage: skip remaining specialties this commit;
#                         file `BudgetExceeded` finding (medium)
#   - daily cap reached:  pause new review jobs until midnight;
#                         logged via `veska doctor pipelines` and
#                         one line in audit.jsonl. Not a Finding;
#                         not human-action-gated. (See SOLO-11 §3.1.)
# USD caps come with hosted LLM providers when they ship. We
# track token caps only today; with the local Ollama provider
# tokens are the meaningful cost. Caps reset at local-midnight.
[review]
enabled               = false
max_tokens_per_commit = 100000
max_tokens_per_day    = 500000

# ─── vuln source (off by default) ────────────────────────────
[vuln_source]
enabled                = false
provider               = "osv"               # "osv" | "ghsa"
refresh_interval       = "24h"

# ─── tracker integration ─────────────────────────────────────
# Default is "none" — no tracker integration. Set to "bd-cli" to
# enable the local `bd` CLI integration; `veska init` probes for
# `bd` on $PATH (same shape as the Ollama probe) and refuses
# silent degradation. We refer to the integration as "the tracker"
# or "the bd-cli tracker"; the brand "Beads" never appears in
# normative prose, audit actor IDs, or config keys.
[tracker]
provider               = "none"              # "none" (default) | "bd-cli"

# ─── auto-link (background post-promotion queue worker; SOLO-11 §4) ────────
# How aggressively the auto-link goroutine writes synthetic edges
# discovered after promotion. "suggest" (default) writes findings of
# kind `auto-link-candidate`; the user accepts/rejects via
# `eng_close_finding`. "apply" writes the edges directly with
# confidence='probable'. "off" disables the worker entirely.
[autolink]
mode                   = "suggest"           # "suggest" (default) | "apply" | "off"

# ─── memory ──────────────────────────────────────────────────
# Daemon-global RSS ceilings. Soft cap pauses the embed worker
# and coalesces tree-sitter reparse; hard cap exits and lets the
# supervisor restart (the crash-loop breaker takes over from
# there). See SOLO-13 §3.3.
[memory]
soft_cap_gib           = 2                   # pause embed worker, coalesce reparse above this
hard_cap_gib           = 4                   # exit and restart above this; raise with care

# ─── supervisor / crash-loop breaker ─────────────────────────
[supervisor]
restart_window         = "10m"               # window for crash-loop counter
max_restarts_in_window = 5                   # exits with code 78 after this; supervisor stops retrying
stable_boot_after      = "60s"               # alive this long → counter resets

# ─── backup ──────────────────────────────────────────────────
[backup]
default_path           = "~/.veska-backups/" # where veska backup create writes by default
auto                   = true                 # daily auto-backup at first idle window after local midnight (SOLO-08 §9.5)
auto_retain            = 7                    # number of auto-backup files retained (older auto-backups pruned; user-initiated backups never auto-pruned)
staleness_warn         = "24h"                # doctor warns if last backup older than this (with auto on, 24h is reachable; lower the alarm window)
required               = false                # if true, daemon refuses to start unless a verified backup exists
pre_migration_keep     = 5                    # auto-snapshots taken by the migration runner (SOLO-08 §10) — last N retained, older pruned
```

### 3.2 Advanced

The keys below exist so a user with a measurement can tune a
specific behaviour. Most users should never edit them; the
defaults are picked against the M1 reference workload and any
divergence should come with a reason.

```toml
# ─── post_promotion_queue (advanced) ──────────────────────────────────
[post_promotion_queue]
high_water             = 10000               # `embed` rows enqueued above this defer instead of pending; promotion proceeds (SOLO-08 §3.4)
low_water              = 8000                # deferred rows transition back to pending below this
done_retention         = "168h"              # 7d; `done` rows GC'd after this. `failed` rows persist until retried.

# ─── writer pools (advanced; SOLO-11 §10, ADR-S0011) ─────────
# database/sql MaxOpenConns=1 serializes transaction acquisition
# at the pool. busy_timeout is the SQLite-level wait when the
# other writer pool holds the OS writer lock. eta_ms returned
# with ErrBusy is derived from sql.DBStats() (WaitDuration /
# WaitCount).
[writer.hot]
busy_timeout_ms        = 5000                # PRAGMA busy_timeout on writeDB.hot connections

[writer.embed]
busy_timeout_ms        = 30000               # PRAGMA busy_timeout on writeDB.embed connections
batch_size             = 256                 # rows per vec_nodes upsert tx (bounds lock-hold time)

[reader]
busy_timeout_ms        = 5000                # PRAGMA busy_timeout on readDB connections

[mcp]
# Default deadline for write tools when the caller omits max_wait_ms.
# eng_add_repo / eng_remove_repo override this in the handler (30s,
# cold-scan-bounded); see SOLO-09 §4.7.
write_max_wait_ms      = 3000
shim_start_timeout_ms  = 3000                # shim wait for socket after asking supervisor to start daemon (SOLO-03 §3.1)

# ─── tokens (advanced) ───────────────────────────────────────
# Pluggable token estimator (SOLO-05 §2.11). Default chars/4 is a
# deliberately approximate heuristic; tune token caps with that
# in mind. ModelHint is recorded in audit lines for truncated
# responses.
[tokens]
provider               = "chars_div_4"       # only impl shipped today
# A future ADR adds tiktoken; until then the field is here so
# the config doesn't have to migrate when it lands.

# ─── retention ───────────────────────────────────────────────
[retention]
deleted_branch_grace   = "168h"              # 7d
finding_closed_grace   = "720h"              # 30d
```

## 4. Resolution order

1. Compile-time defaults.
2. `~/.veska/config.toml` (overlay).
3. Environment variables (overlay).
4. CLI flags (overlay; rare; for one-shot commands).

Restart required for any change. There is no hot reload.

## 5. What is NOT here (and why)

- **OIDC / SAML / SCIM.** No external IdP. Identity is the OS
  user who started the daemon plus an `actor_kind` enum.
- **Replication / canonical mirror.** One machine, one daemon.
- **Per-workspace pipelines / performance / identity.** One daemon,
  one config.
- **AlertNotifier / SLO burn pages.** Out of scope.
- **ExternalSource / typed registry / capability schema.** Plugins
  are Go interfaces; one impl each.
- **Mode selector (`[L]`/`[W]`/`[C]`).** Only one mode exists.

If a knob you want is not here, file an open question (SOLO-OQ)
rather than adding a key in passing.
