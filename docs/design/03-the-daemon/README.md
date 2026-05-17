---
id: SOLO-03
title: "The Daemon — Runtime & Developer Journey"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-04, SOLO-07, SOLO-08, SOLO-09, SOLO-11]
verified: true
verified_date: "2026-05-16"
---

# SOLO-03 — The Daemon

## 1. Purpose

This is the topology view of Engram Solo: one developer, one
machine, one daemon. It names the binaries, draws the runtime,
spells out the lifecycle, and walks an end-to-end developer
journey from edit through commit to drain. Subsystem detail
(domain, storage, MCP, pipelines) lives elsewhere; this doc is the
integration view.

For the reader-friendly **dataflow** view (save / promotion / query as
three small diagrams), see [`PRODUCT.md` §"How it works"](../../PRODUCT.md).
The diagram below is the **process** view — what runs, where, and
how the binaries talk.

## 2. The three binaries

| Binary | Role |
|---|---|
| `veska-daemon` | Long-running process. Owns SQLite, the embedding worker, the MCP socket, the fsnotify watcher, all background goroutines. One per developer machine. |
| `veska` | CLI. Sends control commands to the daemon over the same Unix socket. Some commands (`veska init`, `veska doctor`) run without a daemon. |
| `veska-mcp` | Stdio shim. Editors and agents speak MCP over stdio; the shim proxies frames to the daemon's Unix socket and exits when stdin closes. |

All three ship from one source tree under `cmd/`. The daemon is
**the** process; the other two are thin clients.

## 3. Runtime topology

The full topology with **all six runtime pieces** named — the
PRODUCT.md three-box diagram is a simplification of this:

```
┌──────────────────────────────────────────────────────────────────────┐
│ Developer machine                                                    │
│                                                                      │
│   ┌──────────┐                  ┌────────────────────────────┐       │
│   │ Editor / │  stdio           │  OS-level supervisor        │      │
│   │  agent   │ ──────────┐      │  (launchd | systemd-user |  │      │
│   └──────────┘           │      │   veska supervise)         │      │
│                          ▼      │                             │      │
│                   ┌──────────┐  │  spawns + restarts          │      │
│                   │veska-mcp│  └────────────┬────────────────┘      │
│                   │ (shim,   │               │                       │
│                   │  stdio)  │               ▼                       │
│                   └────┬─────┘     ┌──────────────────┐              │
│                        │ mcp.sock  │                  │              │
│                        └──────────▶│                  │              │
│                                    │                  │              │
│   ┌──────────┐  cli.sock           │  veska-daemon   │              │
│   │  veska  │ ──────────────────▶ │                  │              │
│   │  (CLI)   │ (MCP tools + ctrl   │  ┌─ SQLite       │              │
│   └──────────┘  RPC; SOLO-09 §1.3) │  │  + sqlite-vec │              │
│                                    │  ├─ embed worker │              │
│                                    │  ├─ post-promotion    │              │
│                                    │  │  queue drain  │              │
│                                    │  ├─ fsnotify     │              │
│                                    │  └─ MCP router   │              │
│                                    └────┬───────┬─────┘              │
│                                         │       │ HTTP               │
│                                         ▼       ▼                    │
│                                   ~/.veska/  ┌──────┐               │
│                                               │Ollama│               │
│                                               └──────┘               │
└──────────────────────────────────────────────────────────────────────┘
```

**Six runtime pieces, one stateful component.** The daemon owns
the only state (`~/.veska/`); the other five (CLI, shim,
supervisor, Ollama, editor) are stateless from Engram's
perspective. The supervisor is load-bearing for first-run UX
(§3.1) and crash-loop semantics (§5.6) and is not optional —
the no-supervisor "manual mode" (`veska daemon` from a
terminal) is a dev convenience, not a supported deployment.

There are **two** Unix sockets: `~/.veska/cli.sock` and
`~/.veska/mcp.sock`. They are the same wire protocol; they
differ only in which `actor_kind` the daemon stamps on traffic
that arrives on them. The split is the human-action gate's physical
substrate (SOLO-10 §1.2): the daemon sets `actor_kind` from the
listener that accepted the connection, not from anything in the
request, so an agent cannot present itself as a human by lying
in a header. The CLI binary `veska` connects only to
`cli.sock`; the stdio shim `veska-mcp` connects only to
`mcp.sock`. There are no external network listeners by default.
SQLite is in-process (CGO); sqlite-vec loads as an extension
into the same database file. Ollama runs as a separate user
process the daemon talks to over HTTP — that is the only
outbound connection in the default configuration.

**Permissions and ownership.**

- Both sockets are created with mode `0600` so only the daemon's
  user can connect. The two-socket layout means the listener
  determines `actor_kind` before any request body is read
  (SOLO-10 §1.2), so the gate's discriminator is which file the
  client connected to rather than anything self-declared in the
  request. This catches accidental MCP-side closures from a
  benign editor extension; it does **not** stop same-user code
  from dialling `cli.sock` directly. The OS user is the only
  privilege boundary on the machine — see SOLO-10 §3.1 for the
  full framing.
- The two listeners are bound by the same daemon process; one
  process, two sockets. There is no separate "MCP daemon" or
  "CLI daemon" — that would be a federation surface we do not
  want.
- A user who manually invokes the CLI binary against `mcp.sock`
  (e.g. `veska --socket=$HOME/.veska/mcp.sock ...`) is
  declaring themselves an agent and the human-action gate behaves
  accordingly. The CLI does not expose this knob; you have to
  reach for it deliberately.

Everything durable lives under `~/.veska/`. Backup is
`veska backup create` (SQLite `VACUUM INTO` + tarball; see
SOLO-08 §9). Verification is `veska doctor backup` (see SOLO-13
§2). Reset is `rm -rf ~/.veska/`.

### 3.0 Repo lifecycle and watcher composition

The daemon manages **N registered repos** simultaneously (the
"working set" — typically a service, its SDK, a docs repo). Repos
are not config; they are dynamic state in the `repos` table
(SOLO-08 §3.1).

**Registration.**

- `veska repo add <path>` — registers a repo and atomically:
  installs the `post-commit` and `post-checkout` Git hooks; reads
  the module path (`go.mod`, `package.json`) into `repos.module_path`
  for the cross-repo resolver (SOLO-11 §10); kicks off a cold scan
  in the background.
- `veska repo remove <id>` — removes hooks, deletes the repo's
  rows (cascades via foreign keys), drops watcher state.
- `veska init` in a Git working tree calls `veska repo add` for
  that tree.

**Per-promotion validation.** The post-commit hook calls
`veska promote --repo <path>`. The daemon maps `<path>` to a
registered `repo_id`; if the path is not registered, the hook
returns non-zero with a `cli_command` pointing at
`veska repo add`.

**Watcher composition.** One fsnotify watcher per registered repo,
each with its own debounce queue. Crashes in one repo's watcher
do not affect others.

- **Linux.** At `repo add` time the daemon checks
  `fs.inotify.max_user_watches` against the new repo's expected file
  count plus the running total across already-registered repos. If
  the budget is short, `repo add` exits non-zero with the `sysctl`
  command to raise the limit. **`repo add` also checks the new
  repo's tracked-path count against
  `[watcher].max_paths_per_repo` (DEFAULT 50 000) and the daemon-
  global `[watcher].max_paths_total` (DEFAULT 200 000); a repo
  whose count exceeds either limit refuses to register with a
  pointer at `.veskaignore` to exclude vendored trees
  (`node_modules/`, `vendor/`, build outputs).** This protects
  against the polling-fallback CPU bomb a half-million-path
  monorepo would otherwise produce.
  The daemon never silently grows past the limit. **At runtime**,
  an `inotify_add_watch` returning `ENOSPC` triggers a structured
  warn-level log line (with the affected directory and the current
  vs. needed budget) and a doctor-surface flag. The watcher does
  not silently miss events: the affected repo flips to per-path
  polling fallback (SOLO-11 §1, polling-fallback row in SOLO-13
  §3.6) until `repo doctor` is re-run after the user raises the
  limit. Branch checkouts that briefly grow the file count past
  the budget (vendored deps on a release branch) are caught by
  the post-checkout reseed rather than silent miss.
- **macOS.** No reliable probe; `veska doctor watcher` surfaces
  the per-mount FSEvents budget and recommends polling fallback if
  many large repos are watched.
- **Polling fallback** (per SOLO-11 §1) is per-repo, not global.

**Active state is per-repo.** The active branch lives in
`repos.active_branch`. The active task lives in `tasks.repo_id`
with a per-repo unique partial index. MCP "current repo" resolves
from the caller's CWD (`eng_get_current_repo`); tools that accept
a `repo` arg use it, otherwise default to the current repo.

**Budget composition.** Performance budgets split between
per-repo (cold-scan, parse, watcher) and daemon-global (RSS, MCP
latency, semantic search across `repo: "*"`). The 4 GiB hard cap
is global; `veska repo add` refuses to register a new repo if its
estimated cold-scan would push projected steady-state RSS past the
soft cap. See SOLO-13 §3.

### 3.1 First-run and auto-start

The daemon must be running for `veska-mcp` to do anything. Three
paths from "user just installed the binaries" to "daemon is alive":

1. **`veska init`** — interactive on first run. See §3.2 for
   the full first-run flow (config, embedder probe, service
   register, repo register).
2. **`veska service install`** — service registration only,
   non-interactively. Idempotent.
3. **Manual / dev mode** — `veska-daemon` run from a terminal.
   No supervisor, no auto-restart.

`veska-mcp` (the stdio shim) handles a missing socket cleanly,
matching the user expectation set by other MCP servers without
becoming the daemon's parent:

- If `~/.veska/mcp.sock` is missing or the connect fails, the
  shim:
  1. Checks whether a supervisor unit is registered for the
     daemon (launchd plist, systemd-user unit, or an
     `veska supervise` PID file from the built-in supervisor;
     §5.1).
  2. **If a supervisor is registered**, the shim asks the
     supervisor to start the daemon:
     - macOS: `launchctl kickstart gui/$UID/com.veska.daemon`
     - Linux systemd-user: `systemctl --user start veska-daemon`
     - Built-in: writes a "start" sentinel that
       `veska supervise` polls.
     The shim then waits up to `[mcp].shim_start_timeout_ms`
     (DEFAULT 3000) for the socket to appear; on success it
     proxies the original MCP frame normally. The user sees the
     daemon come up without leaving the editor. **The shim never
     becomes the daemon's parent process** — the supervisor
     starts the daemon, so orphan-process and racy-upgrade
     concerns the prior design called out are still solved.
  3. **If no supervisor is registered**, the shim returns
     `ErrDaemonNotRunning` with `cli_command:
     "veska service install"` so the editor can render a
     one-paste install affordance. Until that runs the editor
     surface is dead, deliberately — the alternative (shim
     forks the daemon) creates the orphan-process problem we
     refused to ship.
- The shim **never** forks `veska-daemon` itself. The daemon's
  parent is always the supervisor.
- For dev mode (no supervisor at all), `veska daemon` from a
  terminal is the documented flow; the shim picks up the socket
  the next time it tries.

### 3.2 The first-run flow

`veska init` is the only command a new user is told to run. It
runs interactively and walks the user through every dependency
the daemon needs. Non-interactive mode (`veska init --yes` or
`veska init --noninteractive`) accepts every default and skips
prompts; the same checks still run and any failure exits non-zero
with a clear `cli_command` to remediate.

**Steps, in order:**

1. **`~/.veska/` layout.** Create the directory, write a default
   `config.toml`, write empty `audit.jsonl`, create `cache/` and
   `models/`. Idempotent: existing files are not overwritten.
2. **Embedder probe.** Check that Ollama is reachable at the
   configured endpoint (`embedder.endpoint`, default
   `http://localhost:11434`). Three outcomes:
   - **Reachable, model present.** Print `embedder: ok
     (nomic-embed-text)`; continue.
   - **Reachable, model missing.** Offer:
     `ollama pull nomic-embed-text`. In interactive mode, prompt
     `Pull now? [Y/n]`; on yes, shell out and stream progress.
     In `--yes`, run unprompted. On failure, exit non-zero with
     the command to retry.
   - **Unreachable.** Print platform-specific guidance:
     - macOS: `brew install ollama && brew services start ollama`
     - Linux: `curl -fsSL https://ollama.com/install.sh | sh`,
       then `systemctl --user enable --now ollama` (or the
       distro equivalent).
     - WSL2: same as Linux, with a note about Windows host
       limitations.
     Then prompt the user to re-run `veska init` after Ollama
     is up. Exit non-zero. We do not try to install Ollama
     ourselves; we do not bundle it (license, binary size,
     supervision); we do not ship an alternative embedder
     (SOLO-05 §2.4 is the spec).
3. **Service register.** Offer `veska service install` (SOLO-03
   §5.1). Default yes. On decline, the user is in "manual mode"
   and must run `veska daemon` themselves; print the dev-mode
   reminder.
4. **Repo register.** If the current working directory is inside
   a Git working tree, offer to run `veska repo add <CWD>`
   (SOLO-03 §3.0). Default yes. The cold scan starts in the
   background; init does not wait for it.
5. **Tracker probe (opt-in).** Default tracker is `none`
   (SOLO-05 §2.1, CONFIG-SURFACE.md `[tracker]`). In interactive
   mode, prompt `Enable tracker integration? [y/N]`; default no.
   On yes, ask which provider (today: only `bd-cli`), then probe
   for `bd` on `$PATH`. Same shape as the Ollama probe in step 2:
   - **`bd` present.** Write `[tracker] provider = "bd-cli"` to
     config; print `tracker: ok (bd-cli)`.
   - **`bd` missing.** Print the install command for the host
     platform (`bd` install one-liner) and exit non-zero with a
     `cli_command` to retry. We do not silently fall back to
     `none` after the user opted in.
   - In `--yes` / `--noninteractive`, the default is `none` —
     opt-in requires a deliberate choice.
6. **Summary.** Print a 7-line summary: data dir, config path,
   embedder status, service status, registered repos, tracker
   status, audit log path. Also print the next thing the user
   should do (typically "open your editor and connect via the MCP
   shim `~/.veska/bin/veska-mcp`").

**Failure modes during init:**

| Step | Failure | Behaviour |
|---|---|---|
| 2 | Ollama unreachable | Print install command for the host platform; exit non-zero. Do NOT continue init. |
| 2 | Ollama reachable, model pull fails (network, disk full, permissions) | Print the failed `ollama pull` command verbatim; exit non-zero. |
| 3 | `veska service install` fails (e.g. systemd-user missing on a non-systemd Linux and no fallback path could be installed) | Print the manual fallback (`veska daemon --supervise` plus instructions for the host's autostart mechanism); init exits non-zero. The fallback path itself is documented in §5.1. |
| 4 | `veska repo add` fails (e.g. inotify watch budget on Linux) | Init exits non-zero with the budget-fix command (SOLO-03 §3.0). |
| 5 | User opted into `bd-cli` and `bd` is missing on `$PATH` | Print the install command; exit non-zero. Config is left at `provider = "none"` so a retry of init does not assume a half-applied state. |

**No silent degradation.** Init does not write a config that
points at an unreachable Ollama endpoint and "let the user figure
it out." If the embedder probe fails, the user is told why and
how to fix it before init returns success.

**`veska doctor embedder` is the same probe.** The check logic
in step 2 lives in `veska doctor embedder`; init invokes it as
a subroutine so the messages and exit codes are identical to the
ones the user will see later from `veska doctor` (SOLO-13 §2).

**No `veska model download`.** Pulling models is an Ollama
operation; the daemon does not wrap it. The probe in init and
`veska doctor embedder` is enough.

**`veska embedder swap <model>`.** Switching the active model
warrants a wrapped command because it is not a pure Ollama
operation: the database's vector geometry (`vec_nodes`'s
declared dim), `database_meta.embedder_*`, the on-disk config,
and every `node_embeddings` row must all move to the new model
together. The user is not in a position to script that
correctly.

The swap is a **multi-step procedure with a daemon-stopped
default, an explicit pre-snapshot, and a refuse-to-start-on-
inconsistency invariant** — not an atomic transaction.
Virtual-table DDL atomicity for `vec_nodes` is implementation-
defined for the sqlite-vec extension, so the in-tx grouping in
step 4 is best-effort. The pre-snapshot in step 3 is the
canonical recovery path.

```
veska embedder swap nomic-embed-text-v1.5
veska embedder swap mxbai-embed-large           # different dim
veska embedder swap --provider=ollama --model=...   # explicit
```

The sequence the daemon runs:

1. Pre-flight: probe Ollama for the new model; read its
   embedding dim from a one-shot test invocation. If the model
   is missing, exit non-zero with the `ollama pull` command.
   **Refuse-to-start invariant.** Before doing any of the
   following, the daemon checks `database_meta.embedder_*`
   against the live `vec_nodes` declared dim. If they disagree
   (a prior swap left the DB inconsistent), the swap aborts
   with instructions pointing at `veska backup restore` and
   the most recent `pre-swap-*` snapshot. The swap will not
   start on top of a broken prior swap.
2. Stop accepting MCP writes (the Unix socket listener stays
   up; reads continue against promoted state). Set
   `degraded_reasons:["embedder_swapping"]` on responses.
3. Take an auto-snapshot via the migration runner's snapshot
   mechanism (SOLO-08 §10) — same lifecycle as a pre-migration
   snapshot, written to `~/.veska-backups/pre-swap-...`. **This
   is the canonical recovery point.**
4. Run the storage transitions on `writeDB.hot`. The daemon
   wraps these in a single `BEGIN IMMEDIATE`/`COMMIT` for the
   rows it controls, but the virtual-table `DROP`/`CREATE`
   participates in that transaction only as far as the
   sqlite-vec extension's behaviour allows. **Treat this as a
   procedure, not an atomic transaction.** If any step fails,
   recovery is "stop the daemon, restore the step-3 snapshot,"
   not "the rollback put us back where we started":
   - Drop `vec_nodes` (the virtual table; the underlying
     storage goes with it).
   - Truncate `node_embeddings` and `node_embedding_refs`.
   - Update `database_meta.embedder_provider`,
     `embedder_model`, `embedder_dim`.
   - Re-`CREATE VIRTUAL TABLE vec_nodes USING vec0(...
     FLOAT[<new_dim>])`.
   - Mark every node's `node_embedding_refs.state = 'pending'`
     so the embed worker re-derives embeddings against the new
     model.
5. Update `~/.veska/config.toml`'s `[embedder]` block to match
   (in-place edit, preserving comments and unrelated keys).
6. Resume MCP writes.
7. Print: "Swapped to <model>; <N> nodes pending re-embed.
   Track progress with `veska doctor post-promotion-queue`."

Properties:

- **Recovery is the pre-swap snapshot.** Step 3's snapshot is
  the canonical recovery path. Step 4's transactional grouping
  is opportunistic — `vec_nodes` DDL rollback is
  implementation-defined for sqlite-vec — and the design does
  not depend on it. The refuse-to-start invariant in step 1
  enforces snapshot-restore as the recovery path on the next
  launch. OQ-S004 measures the in-tx rollback case empirically;
  even a green close does not change the supported recovery.
- **Re-embedding is background.** The embed worker drains
  `pending` rows on its normal cadence. Search responses
  surface `degraded_reasons:["embedding_pending"]` until the
  queue drains. There is no all-or-nothing wait.
- **Model dim mismatches are caught.** Step 1 reads the new
  model's dim; step 4 writes the new `vec_nodes` against that
  dim. The boot consistency check (SOLO-08 §3.3) covers
  subsequent restarts.

The CLI subcommand `veska embedder current` prints the active
provider/model/dim from `database_meta`. `veska doctor
embedder` extends to show the swap state (`idle` |
`swapping` | `re_embedding`).

## 4. Cross-cutting properties

1. **One process, one machine, one user.** No tier above. No tier
   below.
2. **Two Unix sockets, one wire protocol.** `cli.sock` (CLI →
   `actor_kind=human`) and `mcp.sock` (MCP shim →
   `actor_kind=agent`). The listener is the human-action gate's
   physical substrate (SOLO-10 §1.2). One daemon binds both.
3. **Offline-tolerant.** Ollama unreachable → semantic search
   degrades gracefully (`degraded_reasons: ["embedding_pending"]`).
   No internet at all → still works for everything except the
   optional vuln feed.
4. **State is durable across restarts.** The promotion transaction is
   the only path that writes durable graph state. Staging is
   in-memory and lossy by design.
5. **Zero telemetry by default.** OTLP and Prometheus are opt-in
   and surface in `veska doctor egress`.

## 5. Lifecycle

### 5.1 Start

The daemon is normally launched by a session-scoped supervisor:

- **macOS:** a `launchd` agent at
  `~/Library/LaunchAgents/com.veska.daemon.plist`.
- **Linux with `systemd --user`:** a unit at
  `~/.config/systemd/user/veska-daemon.service`.
- **Linux without `systemd --user` (Alpine, NixOS w/o systemd-user
  enabled, plain devcontainers, WSL2 default):** the built-in
  `veska supervise` subcommand. It is a Go-side supervisor in
  the same binary, sharing the crash-loop breaker (§5.6) with
  the launchd / systemd paths. Usage:
  ```
  veska supervise [--pidfile=~/.veska/state/supervise.pid]
  ```
  The user puts that line in their shell rc, an init script, a
  tmux session, or a desktop-environment autostart entry. The
  subcommand maintains a PID file the shim reads to detect the
  registered supervisor (§3.1). It is **not** a bash script;
  the prior 18-line `veska-supervise.sh` is retired. Same
  exit-code discipline (78 = terminal, supervisor halts) as the
  external supervisors. `veska service install` detects the
  platform and writes the right artifact: a `launchd` plist on
  macOS, a systemd unit on systemd-user Linux, or a shell-rc
  snippet that invokes `veska supervise` on Linux without
  systemd-user. Service installer exits 0 only after a working
  supervision path is confirmed; if no autostart hook can be
  installed (the user's distro provides no facility), it prints
  the `veska supervise` line and instructs the user to add it
  manually.
- **Windows:** not supported. WSL2 falls under the Linux
  paths above (typically the no-systemd-user fallback).

`veska service install` writes the appropriate artifact for the
host platform and registers it. `veska service uninstall`
reverses it. `veska doctor service` reports which supervision
path is active and whether the daemon is currently running under
it. The daemon binary is installed to a stable path
(`~/.veska/bin/veska-daemon`) so artifacts do not need to be
rewritten when the binary is upgraded; see §5.5.

On start the daemon:

0. **Opens the log file first** (`~/.veska/logs/daemon.log`)
   before any other initialisation. A panic in step 1–6 must
   leave a diagnostic trail; opening the log later means early-
   start failures are invisible. The supervisor's stderr capture
   is the fallback if step 0 itself fails.
1. Loads config from `~/.veska/config.toml`; probes `~/.veska/`
   filesystem.
2. Checks the broken marker and crash-loop breaker (§5.6).
3. Opens SQLite (`~/.veska/veska.db`); loads sqlite-vec from
   `~/.veska/lib/vec0.<ext>` (SOLO-08 §1.1); runs the migration
   runner (SOLO-08 §10); checks `database_meta` against
   `[embedder]` config; checks `[backup].required`.
4. Brings up embedding worker, post-promotion-queue-drain goroutines, fsnotify
   watcher.
5. Binds both Unix sockets (`cli.sock` and `mcp.sock`) at mode
   `0600`. The MCP router accepts on both; the accepting
   listener determines `actor_kind` (SOLO-10 §1.2).
6. Marks ready; resets the crash-loop counter on stable boot
   (alive ≥ `[supervisor].stable_boot_after`).

Any of the conditions in §5.8 cause exit 78 at the noted step.
No silent degradation.

### 5.2 Run

The daemon is long-running — days-to-weeks per session. It
survives editor restarts. RSS soft and hard caps live in SOLO-13
§3.3 (both unmeasured at write time, gated by M1). The hard cap
is enforced via §5.6.

**Daemon log rotation.** Stderr is redirected by the supervisor to
`~/.veska/logs/daemon.log`. The daemon rotates this file
internally (not via external `logrotate`) at 100 MiB, keeping 5
rotations. On `SIGHUP` the daemon reopens log files — the same
signal an external rotator would use, in case the user wires one
in. Audit log rotation (`audit.jsonl`) is independent and
unchanged (SOLO-08 §3.5).

**Sleep / wake reconciliation.** Laptops sleep. The reconcile
path keeps the MCP listener serving promoted reads from t=0 with
`degraded_reasons: ["wake_reconciling"]`; staging catches up
per-repo as sweeps complete. Reads never block on the sweep.
Both fsnotify backends drop events under sleep:

- **macOS FSEvents** delivers a coalesced "must rescan" event
  on wake but the per-path event stream up to that point is
  lost. We do not rely on the rescan event being delivered
  reliably across long suspends.
- **Linux inotify** queues events while the process is alive;
  when the process is suspended (laptop closed, system
  hibernate), the kernel queue caps at
  `fs.inotify.max_queued_events` and overflow is signalled by
  `IN_Q_OVERFLOW`. Once seen, every watch on the affected
  instance must be re-walked.

The daemon's response is a wall-clock-gap detector plus a
per-repo mtime sweep. A monotonic clock (`time.Now()` against
a rolling baseline) ticks every 5s. When two consecutive ticks
show a gap larger than `[watcher].wake_threshold` (default 30s,
CONFIG-SURFACE) — a near-certain signal of suspend, kernel
freeze, or process pause — the daemon runs `wake reconcile`:

1. Mark every registered repo's staging as
   `degraded_reasons: ["wake_reconciling"]`. **The MCP listener
   continues to serve reads against promoted state from t=0**;
   the wake-reconcile flag is informational and does not block
   the MCP socket.
2. For each registered repo, walk the working tree and compare
   each tracked file's `(path, mtime, size)` with the most
   recent record (the watcher already keeps this for ignore-
   filter caching). Files whose `mtime` or `size` changed are
   treated as if a save event had fired. **Repos sweep in
   parallel**, capped at `[watcher].wake_concurrency` (DEFAULT
   `runtime.NumCPU() / 2`); the multi-repo total is dominated
   by the slowest repo, not the sum.

   **Same-length-same-mtime hazard.** Format-on-save tools
   (gofmt, prettier, ruff) routinely produce same-length
   replacements, and APFS coalesces mtime at second granularity.
   A file edited and saved during suspend may pass the
   `(mtime, size)` comparison as unchanged. The wake sweep
   therefore *also* compares each file's first 64 bytes to the
   last-recorded prefix; a mismatch triggers a reparse even when
   `(mtime, size)` looks clean. Full content hashing on the wake
   path is too expensive for working trees with >50k files; the
   prefix probe is a cheap intermediate. Files that change only
   beyond the first 64 bytes still slip through the wake sweep
   but are caught at the next user save (fsnotify-driven) or the
   next promotion's diff sweep. The known-residual case is
   surfaced via `veska doctor watcher` rather than ignored.
3. Hand each changed file to the parse-on-save pipeline (SOLO-11
   §1) which writes through staging, exactly as a live save
   would.
4. Restart the watcher's underlying handle. On Linux this is a
   fresh `inotify_init` plus add-watches. On macOS it is a new
   FSEvents stream from the current event ID; **if the event ID
   is stale** (FSEvents has aged it out, returning `kFSEventStreamEventIdSinceNow`
   or equivalent error), the daemon falls through to a full
   working-tree rescan via the same path step 2 took, so no
   between-sleep change is silently missed. The wake-threshold
   tick already triggered step 2's per-file mtime/size check; a
   stale FSEvents ID just means we cannot subscribe to the
   incremental stream from the prior event, not that we lost
   coverage of the working tree. Live saves resume against the
   fresh stream.
5. Clear the `wake_reconciling` flag *per repo* as that repo's
   sweep finishes; the editor sees the badge clear repo-by-repo
   rather than waiting for the slowest.

This sweep is bounded by the working tree size and runs entirely
through the read pool plus parse-on-save staging — no database
writes until the user's next promotion triggers them. We do not
attempt to detect *which* files changed during the suspend window
beyond mtime/size; rename detection that would require content
hashing is out of scope for the wake path. The next promotion's diff
sweep (SOLO-11 §2) catches any residual mismatch.

**User-visible symptom.** Between wake and the next promotion, MCP
reads against renamed files may show ghost nodes for the old
path: the rename arrived as a delete + create that the wake
sweep cannot distinguish from "file changed and grew." The
staging entry is updated for the new path; the old path's
staging entry is dropped only when the post-checkout reseed or
the next promotion's diff sweep prunes it. The window is bounded by
"time to next promotion." This is a known limitation; if it surfaces
in practice the fix is `git status --porcelain` rename detection
on wake, but we do not pre-author it.

If the user runs `git checkout` during the suspended window, the
post-checkout hook (SOLO-11 §1.3) fires on resume and the branch-
switch quiescence path takes over from the mtime sweep. The two
paths are reconciled by the staging generation counter, not by
ordering: wake-reconcile parses are tagged with the staging
generation current at parse start (same rule as live saves) and
obey §1.3's rejection invariant. If `PostCheckout` bumps the
generation mid-sweep, in-flight wake parses commit against the
old generation and are dropped at staging-write time; the
quiescence handler's reseed then populates staging on the new
branch. If `PostCheckout` arrives *after* a sweep completes, the
quiescence drop wipes the sweep's writes along with everything
else. Either order is safe; neither path needs to know about the
other.

**Staging-vs-HEAD check at sweep start (closes the
hook-discarded case).** Both the wake-reconcile sweep and the
startup-resync path begin by reading the repo's working-tree
`HEAD` and comparing it to `repos.active_branch`. On mismatch,
the staging generation is bumped *before* any parse runs, the
staging entries from the prior `active_branch` are dropped,
and `repos.active_branch` is updated to the working tree's
current branch. This closes the pathological case (daemon
stopped, user runs `git checkout`, daemon restarted with the
post-checkout hook already discarded by the shell) without
depending on the hook firing. §5.7's Git↔Engram resync handles
the SHA-level recovery; the staging-vs-HEAD check handles the
branch-level one. Both run on the same restart path.

### 5.3 Restart recovery

The daemon may be restarted at any time:

- **Promoted state** — recovered from SQLite as-is. WAL replay (if
  the daemon crashed mid-checkpoint) is handled by SQLite itself.
- **Staging** — discarded. The fsnotify watcher reparses the
  current filesystem state on the next save event. While reparsing,
  MCP responses carry `degraded_reasons: ["staging_recovering"]`.
- **post-promotion queue** — rows in `state = in_progress` are reset to `pending`
  on startup; the drain goroutines pick them up.
- **Git↔Engram resync (§5.7)** — for every registered repo, the
  daemon compares `repos.last_promoted_sha` against the working
  tree's `HEAD` and replays missing commits into the promoted
  graph before serving begins.

A crash mid-promotion is survivable: SQLite's `BEGIN IMMEDIATE` either
committed (the row is durable) or rolled back (the row never
existed). There is no half-state to clean up. The §5.7 replay
covers the residual case where a hardware-level failure between
`COMMIT` and the next WAL checkpoint loses the most recent promotion
while Git already advanced its SHA (SOLO-08 §5.1, "durability
cliff").

### 5.4 Stop

`veska daemon stop` (or SIGTERM):

1. Stop accepting new MCP connections.
2. Drain in-flight requests up to a 30-second deadline.
3. Stop the embedding worker and post-promotion-queue-drain goroutines.
4. Checkpoint the WAL.
5. Close SQLite.
6. Exit 0.

A SIGKILL is also survivable — see 5.3.

### 5.5 Upgrade

**Distribution channel.** V2.0 ships as `tar.gz` from GitHub
Releases — one archive per platform pair (`linux/amd64`,
`linux/arm64`, `darwin/amd64`, `darwin/arm64`). Each archive
contains the three binaries (`veska`, `veska-daemon`,
`veska-mcp`) and the per-platform sqlite-vec extension
(SOLO-08 §1.1); the daemon writes the extension to
`~/.veska/lib/` on first start. **No auto-update; no package
manager dependency.** Homebrew tap and a shell installer
(`curl -fsSL https://veska.sh/install.sh | sh`) are
M5-or-later work and not required for V2.0. macOS releases are
notarised (sqlite-vec dylib + binaries, signed by the same
certificate); Linux releases ship unsigned and are verified
against published SHA-256 checksums.

The user has just downloaded a new release.

1. **Replace the binaries.** `veska upgrade <path>` (or the
   user's package manager) writes the new binaries to
   `~/.veska/bin/veska-daemon.next`, `veska.next`,
   `veska-mcp.next`, then atomically `mv` them into place. The
   running daemon is unaffected by the file replacement —
   already-loaded text is in memory.
2. **Restart the daemon.** `veska service restart` calls
   `launchctl kickstart -k` or `systemctl --user restart` so the
   supervisor stops and re-spawns the daemon under its existing
   unit. SIGTERM → drain → SIGKILL after 30s if needed.
3. **Service unit reconciliation.** `veska upgrade --restart`
   re-runs `veska service install` before kicking the
   supervisor, so any new env var, signal-handling tweak, or
   `KillMode=` change in the new binary's unit template lands
   without the user needing to remember a separate step. The
   reconcile is idempotent; if the unit hasn't changed it
   no-ops.
4. **Migrations.** On the new daemon's start (§5.1), the
   migration runner (SOLO-08 §10) takes an auto-snapshot, runs
   pending migrations in order — each in its own transaction —
   and refuses to start on schema mismatch (exit 78) or
   migration failure. The user is never asked to run
   `veska backup create` manually before an upgrade; the runner
   does it. **Visibility during the snapshot.** A 5 GiB DB can
   block startup for tens of seconds while `VACUUM INTO` runs;
   during that window the daemon writes
   `~/.veska/state/upgrading` with the snapshot start time and
   estimated remaining bytes. The MCP shim reads the marker
   when `mcp.sock` is missing and returns `ErrDaemonStarting`
   with a `cli_command` payload pointing at `veska doctor
   service` so the editor can render "upgrade in progress" rather
   than "daemon unavailable."
5. **CLI versions.** The `veska` CLI is stateless; replacing it
   has no transition. The `veska-mcp` shim is also stateless;
   the editor reconnects via stdio on its own schedule.

`veska upgrade` is one command for all of the above:
`veska upgrade --restart` does the binary path + service
restart; without `--restart` it stages the next-binary files and
prints the restart command. Backup creation is **not** automatic
— the user runs `veska backup create` if they want one (the
daemon will refuse the start in step 3 if it needs one and none
exists).

### 5.6 Crash-loop circuit breaker

The breaker exists for *runtime* crashes — the daemon came up,
ran, and exited non-78 (e.g. RSS hard cap, SOLO-13 §3.3; panic
in a core goroutine). Refuse-to-start cases (sqlite-vec missing,
schema mismatch, ErrEmbedderMismatch, etc.) all exit 78 and the
supervisor halts on its own; they do **not** count against the
breaker. §5.8 is the canonical matrix.

- Counter is a row in the `daemon_state` table (SOLO-08 §3.3a)
  with key `restart_count`. The counter increments on each
  start that ran past the migration runner but exited before
  `[supervisor].stable_boot_after` (default 60s). Increment is a
  short transaction on `writeDB.hot` before any other goroutine
  starts, so SQLite's atomicity gives us the consistency the
  prior plain-file design hand-waved. The breaker reads the
  counter inside the same transaction it would increment, so
  there is no read-modify-write race even under a fast restart
  loop.
- **Two state surfaces, two readers.** The authoritative breaker
  state is the SQLite row, read by the daemon at start. The
  built-in `veska supervise` cannot read SQLite when the daemon
  is down, so it reads the `~/.veska/state/broken` marker file
  (and `CRASH-LOOP-TRIPPED.txt`) as its halt signal. The daemon
  is responsible for writing both surfaces in step §5.6's exit
  path; `veska supervise` is responsible for honouring the
  marker. launchd / systemd-user use exit code 78 directly and
  do not read the markers — they halt because the daemon told
  them to.
- ≥ `[supervisor].max_restarts_in_window` (default 5) starts in
  `[supervisor].restart_window` (default 10m) ⇒ daemon writes
  `~/.veska/state/broken` (a sentinel file used only for the
  CLI banner check below; the authoritative breaker state is
  the SQLite row), logs `veska_code: "ErrCrashLoop"`, exits 78.
  Supervisor halts.
- Stable boot (alive ≥ `stable_boot_after`) resets the counter
  and removes the `broken` marker.
- The marker blocks daemon start only. CLI repair commands
  (`veska doctor reset-crash-loop`, `veska backup restore`,
  `veska embedder swap`'s daemon-stopped variant) work without
  the daemon.
- `veska doctor` and `veska doctor service` surface the marker
  as exit 2 with recent log paths.
- User clears manually: `veska doctor reset-crash-loop`.

**Notification on trip.** A tripped breaker is otherwise
silent — the daemon is dead, the supervisor has stopped trying,
and the editor's MCP connection just times out. Before exiting
78, the daemon writes a sentinel notification through whichever
of these is available, in this order, and stops at the first
that succeeds:

1. **macOS:** `osascript -e 'display notification "Engram daemon stopped (crash-loop). Run: veska doctor reset-crash-loop" with title "Engram"'`
2. **Linux with `notify-send`:** `notify-send -u critical "Engram" "Daemon stopped (crash-loop). Run: veska doctor reset-crash-loop"`
3. **Always (fallback):** write `~/.veska/state/CRASH-LOOP-TRIPPED.txt` with the timestamp, the last 50 log lines, and the remediation command.

The notifier is best-effort; failure to deliver does not block
the exit. The fallback file is the contract — it is the place a
user (or a support bundle) can reliably find evidence that the
breaker tripped without consulting the supervisor's own state.

**Refuse-to-start exits also write the marker.** §5.8 conditions
exit 78 *before* the breaker counter increments, so the prior
"broken marker only on breaker trip" shape gave two equally-
fatal failure modes (breaker vs §5.8) two completely different
debug surfaces. The fix: every exit-78 path in §5.8 writes
`~/.veska/state/broken` with the §5.8 row reason, the
recommended remediation, and the failing-binary version *before
exiting*. The breaker (this section) writes the same marker
plus the additional `CRASH-LOOP-TRIPPED.txt` sentinel that
distinguishes a breaker-trip from a single refuse-to-start.

**CLI banner check is the first thing every `veska` invocation
does.** Before flag parsing, before subcommand dispatch, before
any RPC: the CLI checks for `~/.veska/state/CRASH-LOOP-TRIPPED.txt`
and `~/.veska/state/broken`. If either is present, the CLI
prints a one-line banner to stderr naming the file, the
trip/refuse timestamp, and the remediation command (the breaker
banner suggests `veska doctor reset-crash-loop`; the
refuse-to-start banner suggests `veska doctor` for the §5.8
diagnosis). The banner fires for **every** subcommand including
`veska --version` and `veska doctor`. The marker survives
until the user clears it (`reset-crash-loop` for the breaker
case; the daemon's own clean start clears the §5.8 marker once
the underlying refuse-to-start condition is fixed). The
notification path on macOS or Linux-with-`notify-send` is
best-effort; the banner is the guarantee.

### 5.7 Git↔Engram resync on startup

Application crashes, SIGKILLs, and clean restarts can leave
`last_promoted_sha < HEAD` whenever the user committed without a
live daemon. The default `synchronous = FULL` (SOLO-08 §5.1)
makes hardware-induced loss vanishingly rare; resync still runs
unconditionally so the daemon catches up after any down period.

**Sequence.** After the migration runner finishes (SOLO-08 §10)
but before the MCP listener accepts connections, the daemon:

1. Reads `repos` from `readDB`. For each registered repo, capture
   `(repo_id, last_promoted_sha, active_branch)`.
2. Reads the repo's working-tree `HEAD` via `git rev-parse HEAD`.
3. **Three cases:**

   | Comparison | Action |
   |---|---|
   | `HEAD == last_promoted_sha` | No work. |
   | `last_promoted_sha` is an ancestor of `HEAD` (`git merge-base --is-ancestor`) | **Replay path.** Walk `git log <last_promoted_sha>..HEAD --reverse`; for each commit, run the same parse-and-promotion pipeline used by the post-commit hook (SOLO-11 §1, §2). Emit one `promotion_id` per commit. |
   | `last_promoted_sha` is **not** reachable from `HEAD` (force-push, branch deletion, history rewrite) | **Divergent path.** Log `veska_code: "ErrPromotionDivergent"` with the diverged SHA. Do **not** rewrite history. Set `repos.last_promoted_sha = HEAD` after a fresh full reparse of the working tree (the same flow as `veska repo add`'s cold scan, scoped to one repo). Findings anchored to commits unreachable from `HEAD` remain in the database under their original `branch` value; they are pruned only by `veska gc --branches` once the branch they belong to is gone. |

4. Cross-branch state — a sibling branch advanced while another
   was active — is **not** chased. Engram only resyncs the repo's
   *currently checked-out* branch on startup. Sibling branches
   resync the next time the user `git checkout`s them (SOLO-11
   §1.5 branch-switch quiescence).

**While replay runs.** MCP responses carry
`degraded_reasons: ["startup_resync"]` and the listener is up
but write tools return `ErrDaemonStarting` until the resync
finishes. Reads against promoted state proceed; they reflect the
pre-resync graph until each commit lands. The replay runs on
the promotion pipeline's normal path through `writeDB.hot`
(ADR-S0011), so it benefits from the same atomicity guarantees
as a live promotion.

**Live saves during resync.** The fsnotify watcher comes online
in §5.1 step 4, *before* the resync goroutine fires. Saves
during the resync window land in `StagingArea` normally; MCP
read tools see (pre-resync promoted) + (live staging) with the
same `startup_resync` degraded flag. Resync's promotion-replay
pipeline replays already-committed Git history and does not
touch staging, so live edits are not overwritten. When resync
completes, the promoted state advances; staging stays. The next
`git commit` promotions through the staged delta against the now-
caught-up promoted state.

**Bounded work.** Replay is at worst the same work the
post-commit hook would have done if the daemon had been alive.
A user who left the daemon down for a long refactor branch may
see a multi-second startup; surface that in `veska doctor`.
The replay runs in commit order, so a partial replay (daemon
killed mid-resync) leaves `last_promoted_sha` advanced to the
last fully-promoted commit and the next start picks up where this
one left off.

**Branch GC piggybacks on resync exit.** Once resync finishes
(or runs as a no-op), the daemon runs `veska gc --branches`
(SOLO-08 §4) before opening the MCP listener. Branch deletions
that happened while the daemon was down are reaped here; the
same routine fires after each wake-reconcile sweep (§5.2).
There is no nightly cron — laptops sleep through wall clocks.

**Surface.** `veska doctor` reports replay state under
`pipelines`:

```jsonc
"resync": {
  "state":        "running",          // "idle" | "running" | "diverged"
  "repo_id":      "...",
  "from_sha":     "<last_promoted_sha>",
  "to_sha":       "<HEAD>",
  "commits_total": 12,
  "commits_done":  5
}
```

Codes: `ok`, `resync_running` (degraded; clears on completion),
`resync_diverged` (broken-shape; user should investigate the
log line).

### 5.7a Child processes the daemon spawns

The daemon shells out for a small number of well-bounded tasks.
Inventoried here so process budgets, signal handling, and
zombie reaping are explicit rather than implicit:

| Caller | Command | When | Lifecycle |
|---|---|---|---|
| Startup resync (§5.7) | `git rev-parse HEAD`, `git merge-base --is-ancestor`, `git log <range>` | Every daemon start, per registered repo | Foreground; daemon waits with a 5s deadline; SIGTERM-on-shutdown propagates |
| Hook runner | `veska hook-runner post-commit` (the daemon does NOT spawn this — Git does, via the installed hook) | On `git commit` / `git checkout` | The daemon is the RPC target, not the parent |
| Crash-loop notify (§5.6) | `osascript` (macOS) or `notify-send` (Linux) | At most once per breaker trip | Best-effort; failure does not block the daemon's exit |
| Backup (§9.1, SOLO-08) | `tar -czf ...` after `VACUUM INTO` | On `veska backup create` and pre-migration auto-snapshot | Foreground; daemon waits with a 600s deadline; SIGTERM aborts the backup cleanly |
| Init (§3.2) | `ollama pull <model>` | Only during `veska init` (the CLI runs this, not the daemon) | The daemon is not the parent |

**Signal propagation.** On daemon SIGTERM (§5.4 stop), every
spawned child receives SIGTERM via the daemon's `Cmd.Process`
group; if a child is still running 5 seconds after the daemon
finishes its drain, it is sent SIGKILL. Zombie reaping is
automatic via Go's `os/exec` — every `Cmd.Wait()` call closes
out the child correctly. There is no user-visible "veska has
spawned a process you have to kill" footgun.

**Process budget.** Steady-state child count is 0; peak (during
a startup-resync of a multi-repo working set) is 1 child per
registered repo for the duration of `git rev-parse`/`git log`.
The OS file-descriptor budget (SOLO-13 §3.3) covers this.

### 5.8 Refuse-to-start matrix

Canonical home for every condition that prevents the daemon from
serving. **All exit 78** ("stop, do not retry"); the supervisor
halts and the user remediates. None increment the crash-loop
breaker (§5.6) — 78 is terminal, not retry-eligible.

| # | Reason | Detected at | Stage in §5.1 | Remediation |
|---|---|---|---|---|
| 1 | `~/.veska/state/broken` marker present | start, before any work | step 2 | `veska doctor reset-crash-loop` after investigating the prior error log |
| 2 | sqlite-vec extension missing or unloadable | DB open | step 3 | install the extension; restart |
| 3 | Schema `current < min_schema` (binary too new) | migration runner | step 3 | downgrade binary, or restore a newer backup |
| 4 | Schema `current > max_schema` (binary too old) | migration runner | step 3 | upgrade binary, or restore a pre-upgrade backup |
| 5 | Migration N failed mid-transaction | migration runner | step 3 | fix migration / downgrade binary / restore the verified pre-migration snapshot (SOLO-08 §10.4) |
| 6 | Pre-migration auto-snapshot failed | migration runner | step 3 | free disk / fix permissions; restart |
| 7 | `migration_sha` recorded ≠ binary's embedded sha (tampering) | migration runner | step 3 | investigate; do not blindly clear |
| 8 | `ErrEmbedderMismatch` — `[embedder]` config disagrees with `database_meta.embedder_*` | post-migration | step 3 | `veska embedder swap <model>` (SOLO-03 §3.2), or revert the config |
| 9 | `~/.veska/` on NFS or unsupported fs (SQLite+WAL correctness) | start | step 1 | move data dir to a supported local fs; set `VESKA_HOME` |
| 10 | `[backup].required = true` and no verified backup found | start | step 3 | `veska backup create`; restart |

**Breaker-eligible exits** (non-78, run-after-start):

| # | Reason | Behaviour |
|---|---|---|
| A | RSS exceeded `[memory].hard_cap_gib` (SOLO-13 §3.3) | exit non-zero; supervisor restarts; breaker counts |
| B | Panic in a core goroutine (promotion, MCP router, watcher) after start | same |

§5.6 is where ≥ 5 of (A) or (B) in 10 minutes flips to a §5.8 row 1 (broken-marker) on the next start.

The marker blocks daemon start only; CLI repair commands run
without the daemon.

## 6. The developer journey

The flow on a single machine:

1. **Edit** — fsnotify fires on save. The watcher hands the path
   to the parser; the parser produces nodes/edges; staging is
   updated. End-to-end: ms to a couple hundred ms for typical
   files.
2. **Query from editor** — MCP tools see the staging overlay
   immediately. `find_symbol`, `get_call_chain`, etc. read
   staging-on-promoted.
3. **Commit** — `git commit` triggers the post-commit hook. The
   hook runs `veska promote` over the Unix socket. The daemon
   promotes staging to SQLite in one transaction and enqueues
   post-promotion queue work. The hook returns. Budgets in SOLO-13 §3.1 (split
   typical vs. refactor commit; both unmeasured at write time,
   gated by M1).
4. **Drain** — embedding, auto-link, finding revalidation run
   asynchronously. None of these block the hook. Findings surface
   in the editor when they're ready.

### 6.1 Commit-promotion sequence

The full sequence — promotion SQL, post-promotion queue enqueue, async drain — lives
in **SOLO-11 §2** (and the failure handling in §2.2). The shape
the daemon owns is just: hook calls `Promote` over `cli.sock`,
daemon runs one `BEGIN IMMEDIATE` transaction on `writeDB.hot`,
hook returns inside the SOLO-13 §3.1 budget, async consumers
drain `post_promotion_queue` independently.

## 7. Failure modes

| Failure | What happens | What recovers it |
|---|---|---|
| Daemon crashes mid-promotion | SQLite either committed (durable) or rolled back (no partial state). Next commit retries. | Automatic. |
| SQLite database locked by an external writer (e.g. user-opened `sqlite3` shell) | `BEGIN IMMEDIATE` blocks until the hot pool's `busy_timeout` (5s) expires; promotion then fails. | Close the external connection; `veska promote --retry` re-runs the hook. |
| Promotion queued behind in-flight MCP writes | Hook's promotion transaction waits on the `writeDB.hot` connection pool (SOLO-11 §10, ADR-S0011). In typical use the pool is idle. | Automatic. |
| Embedding worker crashes | Restarted by the daemon supervisor. Pending refs stay `pending`. Semantic search returns `degraded_reasons: ["embedding_pending"]`. | Automatic on restart. |
| Ollama unreachable | Embedding worker logs and backs off. Pending queue grows. | Self-heals when Ollama is back; the worker resumes. |
| Disk full | Promotion fails; hook returns non-zero; daemon refuses new MCP writes until disk pressure clears. | `veska doctor disk` reports; user frees space. |
| sqlite-vec extension missing | Daemon refuses to start; clear error with install instructions. | Install the extension, restart. |
| WAL grows unboundedly | Daemon checkpoints the WAL when idle (no writes for 5s). `PRAGMA wal_autocheckpoint = 1000` (pages). | Automatic. |
| Editor connects during staging-recovering window | MCP responses carry `degraded_reasons: ["staging_recovering"]` and serve the promoted view. | Resolves when fsnotify reparse completes (sub-second to seconds). |
| RSS exceeds 4 GiB hard cap | Daemon exits; supervisor restarts. Crash-loop breaker (§5.6) trips after 5 restarts in 10 minutes. | `veska doctor reset-crash-loop` after fixing the underlying cause; see SUPERVISION-RUNBOOK.md §4. |
| `veska-mcp` cannot reach the socket | Returns `ErrDaemonNotRunning` with a `cli_command` to start the service. Editor surfaces the message. | User runs `veska service start` (or `veska service install` if not yet registered). See §3.1. |
| Schema migration pending | Daemon takes an auto-snapshot (SOLO-08 §10) and runs the migration in one transaction. | Automatic. |
| Schema migration fails | Transaction rolls back; daemon refuses start with exit 78 and a pointer to the verified pre-migration snapshot. | Fix the migration and restart, or downgrade the binary, or restore from snapshot. |
| Schema mismatch (binary too new or too old for on-disk schema) | Daemon refuses start with exit 78 and the recovery commands. | Use a binary whose `min_schema..max_schema` range covers the on-disk version. |
| Log file `~/.veska/logs/daemon.log` exceeds rotation threshold | Daemon rotates internally at 100 MiB; keeps 5 rotations. | Automatic. `SIGHUP` to force-reopen for external rotators. |

## 8. What this doc does not cover

- The SQLite schema → SOLO-08.
- Domain entities and ports → SOLO-04, SOLO-07.
- The MCP tool catalog → SOLO-09.
- Save vs. promotion pipeline detail → SOLO-11.
- Performance budgets and observability → SOLO-13.
