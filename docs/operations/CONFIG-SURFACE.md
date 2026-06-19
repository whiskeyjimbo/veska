---
title: "Configuration Surface"
status: reference
last_reviewed: 2026-06-19
related: [ARCHITECTURE]
verified: true
verified_date: "2026-06-19"
---

# Configuration Surface

> **Loader status.** `internal/platform/config` ships a real TOML
> loader (`config.go`): `Load()` resolves
> **defaults < `~/.veska/config.toml` < env vars** and `Validate()`
> enforces the tracing and embedder rules. The `Config` struct parses
> `[daemon]`, `[logging]`, `[metrics]`, `[tracing]`, `[storage]`,
> `[watcher]`, `[embedder]`, `[post_promotion_queue]`, `[budget]`,
> `[llm_generator]`, `[review]`, `[summary]`, `[backup]`,
> `[vuln_source]`, `[promotion]`, `[wiki]`, `[autolink]`, and `[blast]`.
> **Not yet parsed** (documented below for forward planning, decoded as
> raw TOML but not bound to a struct field): `[parser]`, `[branch_pk]`,
> `[save]`, `[tracker]`, `[mcp]`, `[memory]`, `[supervisor]`,
> `[writer.*]`, `[reader]`, `[tokens]`, `[retention]`. Treat any key in
> those sections as a planning placeholder, not a live knob.

The whole story. One file (`~/.veska/config.toml`) plus a handful
of environment variables. No per-workspace config files, no
identity policy, no replication policy, no mode selectors.

## 1. Files Veska reads

| File | Purpose |
|---|---|
| `~/.veska/config.toml` | Daemon config. Created by `veska init` if absent. |
| `~/.veska/audit.jsonl` | Append-only audit log. Owned by Veska. |
| `<repo>/.veskaignore` | Per-repo ignore patterns. Plain `.gitignore` syntax. |
| `<repo>/.beads/current_task` | Active-task pin if the `bd-cli` tracker integration is on. |

That's it. Backup is `veska backup create`, not a
`tar` of the live directory - tarring a running SQLite database
captures inconsistent WAL state.

## 2. Environment variables

| Var | Purpose | Default |
|---|---|---|
| `VESKA_HOME` | Daemon data root. | `~/.veska` |
| `VESKA_CONFIG` | Override the config file path. | `$VESKA_HOME/config.toml` |
| `VESKA_DEBUG` | Enable debug-level logging when set. | unset |
| `VESKA_EMBEDDER` | Force the embedder election. `ollama` is the only override that probes a network embedder; otherwise the daemon elects model2vec (or the static-v2 fallback). | unset (elect model2vec) |
| `VESKA_EMBED_MODEL` | Ollama model name used when `VESKA_EMBEDDER=ollama`. Overrides `embedder.model`. | `nomic-embed-text` |
| `VESKA_OLLAMA_URL` | Ollama endpoint used when the Ollama embedder is elected. Overrides `embedder.endpoint`. | `http://localhost:11434` |
| `VESKA_VECTOR_BACKEND` | Vector store backend: `memory` (memvec) or `usearch` (HNSW). Overrides `storage.vector_backend`. | `memory` |
| `VESKA_OTLP_ENDPOINT` | OTLP exporter target. Enables tracing if set. Overrides `tracing.otlp_endpoint`. | unset |
| `VESKA_PPROF` | Bind a pprof listener (host:port) for profiling. | unset |
| `VESKA_HUB_THRESHOLD` | Blast-radius hub-degree gate. Integer; negative disables the gate. Overrides `blast.hub_degree_threshold`. | `50` |
| `VESKA_AUTOLINK_THRESHOLD` | Auto-link minimum similarity, `[0, 1]`. Overrides `autolink.threshold`. | `0.60` |
| `VESKA_AUTOLINK_TOPK` | Auto-link per-source candidate cap (> 0). Overrides `autolink.top_k`. | `5` |

Env vars override file values. CLI flags override env. No hot
reload; restart for changes to take effect.

## 3. `~/.veska/config.toml`

A complete example with defaults. The surface is split: a
**common** set users tune routinely (top of the file) and an
**advanced** set that exists for tuning corner cases and that
most users will never touch (bottom). The split is documentation,
not enforcement - TOML doesn't care, but it lets a user opening
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
listen          = "127.0.0.1:9090"           # only bound when enabled = true

# ─── tracing (opt-in; off by default) ────────────────────────
# Both `enabled = true` AND `otlp_endpoint` (or
# VESKA_OTLP_ENDPOINT env var) MUST be set; setting one without
# the other is a config error caught at startup. Sample ratio
# is set by the operator at opt-in time; there is no default
# that ships traces.
[tracing]
enabled         = false
otlp_endpoint   = ""                         # e.g. "http://localhost:4318" - required when enabled
sample_ratio    = 1.0                        # set at opt-in time; lower for noisy local profiling

# ─── storage ─────────────────────────────────────────────────
[storage]
db_path                = "~/.veska/veska.db"
journal_mode           = "WAL"               # do not change
synchronous            = "FULL"              # "FULL" (default) | "NORMAL".
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
wake_threshold         = "30s"               # monotonic-clock gap that triggers wake reconcile
wake_tick              = "5s"                # cadence of the wake-detector tick
wake_concurrency       = 0                   # parallel per-repo wake-reconcile sweeps; 0 = runtime.NumCPU()/2 (floor 1)
max_paths_per_repo     = 50000               # repo add admission ceiling
max_paths_total        = 200000              # daemon-global path-watch ceiling

# ─── budgets ─────────────────────────────────────────────────
[budget]
refactor_commit_threshold_symbols = 5000     # promote count above which the refactor budget applies

# ─── branch-PK row growth (OQ-S006 / ADR-S0013) ──────────────
[branch_pk]
max_growth_per_branch_pct = 10               # % of trunk row count per active branch; M0 spike compares against this to trip ADR-S0013

# ─── save large-file fallback ────────────────────────────────
[save]
large_file_threshold_loc = 1500              # files above this reparse on a background goroutine

# ─── embedder ────────────────────────────────────────────────
# NOTE: the *active* embedder is chosen at boot by the election in
# `electEmbedder` (builder.go), NOT by `provider` here. The daemon
# elects model2vec (potion-code-16M) when available, else the in-binary
# static-v2 fallback; Ollama is used only when VESKA_EMBEDDER=ollama.
# `provider` is currently not consumed for selection - it is retained
# for the planned config-driven override. `endpoint`/`model` apply when
# the Ollama path is elected (env vars VESKA_OLLAMA_URL / VESKA_EMBED_MODEL
# override them); `rate_per_sec` and `batch_size` are live today.
[embedder]
provider               = "ollama"            # not consumed for election; see note above
endpoint               = "http://localhost:11434"
model                  = "nomic-embed-text"
dim                    = 768
rate_per_sec           = 10
batch_size             = 32

# ─── llm generator (review pipeline; off by default) ─────────
# Ships only the local Ollama generator. Hosted providers
# (Anthropic, OpenAI, Gemini, openai_compatible) and the
# environment-variable key resolution they require come behind a
# future ADR. Setting `provider` to anything
# other than "ollama" causes the daemon to refuse to start.
[llm_generator]
enabled                = false
provider               = "ollama"            # "ollama" only.
endpoint               = "http://localhost:11434"
model                  = "llama3.1:8b"
timeout                = "60s"

# ─── review pipeline ─────────────────────────────────────────
# Hard halts. Behavior:
#   - per_commit overage: skip remaining specialties this commit;
#                         file `BudgetExceeded` finding (medium)
#   - daily cap reached:  pause new review jobs until midnight;
#                         logged via `veska doctor pipelines` and
#                         one line in audit.jsonl. Not a Finding;
#                         not human-action-gated. 
# USD caps come with hosted LLM providers when they ship. We
# track token caps only today; with the local Ollama provider
# tokens are the meaningful cost. Caps reset at local-midnight.
[review]
enabled               = false
max_tokens_per_commit = 100000
max_tokens_per_day    = 500000

# ─── vuln source (M7) ──────────────────────────
# Drives the `vuln-scan` promotion check. Two layers: the bare
# schema zero-value (empty `provider`) is off - the daemon falls
# back to the no-op NullVulnSource - BUT `veska init` writes this
# block with `provider = "osv"` ENABLED by default, so a fresh
# install ships with OSV scanning on. Opt out with `veska init
# --no-vuln` (or answer "no" at the interactive prompt). Any
# value other than "" / "osv" is a fatal startup error.
# `refresh_interval` overrides the advisory-cache refresh
# cadence (Go duration); empty falls back to the 24h default.
[vuln_source]
provider               = ""                  # "" (schema zero-value, off) | "osv" (what `veska init` writes)
refresh_interval       = "24h"

# ─── promotion checks (M7) ───────────────────────────────────
# `disabled_checks` names structural checks to skip; each entry
# matches a check's Name() - "dead-code", "contract-drift",
# "vuln-scan", "secrets-scan". An empty list (the default) keeps
# every registered check on. `vuln-scan` is only registered when
# `[vuln_source]` is configured; `secrets-scan` ships on by
# default and is the usual reason to set this.
[promotion]
disabled_checks        = []                   # e.g. ["secrets-scan"]

# ─── tracker integration ─────────────────────────────────────
# Default is "none" - no tracker integration. Set to "bd-cli" to
# enable the local `bd` CLI integration; `veska init` probes for
# `bd` on $PATH (same shape as the Ollama probe) and refuses
# silent degradation. We refer to the integration as "the tracker"
# or "the bd-cli tracker"; the brand "Beads" never appears in
# normative prose, audit actor IDs, or config keys.
[tracker]
provider               = "none"              # "none" (default) | "bd-cli"

# ─── auto-link (background post-promotion queue worker) ────────
# How aggressively the auto-link goroutine writes synthetic edges
# discovered after promotion. "suggest" (default) writes findings of
# kind `auto-link-candidate`; the user accepts/rejects via
# `eng_close_finding`. "apply" writes the edges directly with
# confidence='probable'. "off" disables the worker entirely.
#
# threshold / top_k tune the candidate computation. Defaults
# are calibrated against the gate-3 nomic-embed-text fixture; a different
# embedder or repo layout is the reason to change them. threshold is a lower
# bound on the higher-is-closer similarity 1/(1+L2dist) and is only meaningful
# on L2-normalized embeddings.
[autolink]
mode                   = "suggest"           # "suggest" (default) | "apply" | "off"  (mode not yet consumed)
threshold              = 0.60                 # minimum similarity to emit a candidate; range [0, 1]
top_k                  = 5                    # per-source candidate cap; must be > 0

# ─── blast radius (graph BFS heuristics) ────────
# hub_degree_threshold gates BFS expansion through high-degree "registry"
# nodes (cobra rootCmd, http muxes): nodes with more neighbors than this are
# reported but not expanded through, so a blast radius isn't drowned in
# framework fan-out. A negative value disables the gate (legacy
# expand-through-everything); 0 is rejected so the disable intent is explicit.
[blast]
hub_degree_threshold   = 50

# ─── memory ──────────────────────────────────────────────────
# Daemon-global RSS ceilings. Soft cap pauses the embed worker
# and coalesces tree-sitter reparse; hard cap exits and lets the
# supervisor restart (the crash-loop breaker takes over from
# there).
[memory]
soft_cap_gib           = 2                   # pause embed worker, coalesce reparse above this
hard_cap_gib           = 4                   # exit and restart above this; raise with care

# ─── supervisor / crash-loop breaker ─────────────────────────
[supervisor]
restart_window         = "10m"               # window for crash-loop counter
max_restarts_in_window = 5                   # exits with code 78 after this; supervisor stops retrying
stable_boot_after      = "60s"               # alive this long → counter resets

# ─── wiki ────────────────────────────────────────────────────
# The developer-wiki Markdown pages (hot_zones.md + entry_points.md).
# Off by default - the product contract is that veska writes no
# files into user repos. Set write_pages = true to materialize the
# pages under <repo>/docs/veska/ on every promotion. The eng_get_hot_zone
# and eng_get_entry_points MCP tools serve the same data either way
# .
[wiki]
write_pages            = false

# ─── backup ──────────────────────────────────────────────────
# Retention policy applied by `veska backup prune` to user-initiated
# backup tarballs. KeepMinCount most-recent backups are always kept
# regardless of age; older ones are deleted once they exceed
# keep_max_age (subject to keep_min_count). Auto-pre-migration
# snapshots taken by the upgrade runner are managed separately.
[backup]
keep_min_count         = 3                    # most-recent backups always kept
keep_max_age           = "30d"                # delete user backups older than this (normalized to hours)
```

### 3.2 Advanced

The keys below exist so a user with a measurement can tune a
specific behavior. Most users should never edit them; the
defaults are picked against the M1 reference workload and any
divergence should come with a reason.

```toml
# ─── post_promotion_queue (advanced) ──────────────────────────────────
[post_promotion_queue]
high_water             = 10000               # `embed` rows enqueued above this defer instead of pending; promotion proceeds
low_water              = 8000                # deferred rows transition back to pending below this
done_retention         = "168h"              # 7d; `done` rows GC'd after this. `failed` rows persist until retried.

# ─── writer pools (advanced) ─────────
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
# cold-scan-bounded)
write_max_wait_ms      = 3000
shim_start_timeout_ms  = 3000                # shim wait for socket after asking supervisor to start daemon

# ─── tokens (advanced) ───────────────────────────────────────
# Pluggable token estimator. Default chars/4 is a
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

If a knob you want is not here, file an issue
rather than adding a key in passing.
