---
id: SOLO-17
title: "Lifecycle and Operations — Schema Migrations, Upgrade, Restore, Install"
status: draft
version: 0.1.0
last_reviewed: 2026-05-09
related: [SOLO-03, SOLO-08, SOLO-13, SOLO-16]
---

# SOLO-17 — Lifecycle and Operations

## 1. Purpose

Engram runs on a developer's laptop. The substrate is one SQLite
file plus a handful of supporting files under `~/.veska/`. That
single file is the user's only copy of their structural ground
truth — there is no upstream, no replica, no service tier to
fall back to. The cost of getting lifecycle wrong on a laptop is
the user's graph.

This section is the normative spec for the operational surfaces
that other docs assume but never own:

- **Schema migrations** (§2) — applying a new daemon binary's
  schema to an existing `veska.db`.
- **Upgrade during run** (§3) — what happens when the user
  replaces the binary while the daemon is up.
- **Backup and restore** (§4) — how `veska backup` and
  `veska restore` interact with the substrate.
- **Install and uninstall** (§5) — what `veska init` writes to
  the filesystem and what `veska uninstall` removes.

Substrate mechanics (WAL, schema rows, vec0 shadow tables) live
in SOLO-08. The doctor surface that reports state lives in
SOLO-13. This file owns the *transitions* between states.

## 2. Schema migrations

### 2.1 Forward-only, versioned, gapless

Migrations are **forward-only**. There is no `down` script and
no automated rollback. A botched migration is recovered from a
backup (§4), not by replaying inverse SQL.

| Property | Value |
|---|---|
| File names | `infrastructure/sqlite/migrations/NNNN_<short_name>.sql` |
| Numbering | Zero-padded 4-digit, sequential, gapless. Numbers never reused. |
| Format | One SQL file per migration. Multiple statements separated by `;`. No conditional logic. |
| Tracking | One row per applied migration in `schema_migrations(version INTEGER PK, applied_at INTEGER, sha256 TEXT)`. |
| Order | Strict numeric order. Skipping a number is a startup failure. |

The `sha256` column hashes the migration file's content at the
time it was applied. On daemon start, the recorded hash is
compared against the file shipped with the current binary. A
mismatch is `ErrSchemaTampered` (SOLO-16) — a migration that was
applied is no longer the same SQL on disk. The daemon refuses to
start.

### 2.2 Daemon-start migration flow

```
daemon start
  → open writeDB.hot connection
  → BEGIN IMMEDIATE
      read max(version) from schema_migrations (or 0 if table absent)
      for each migration file with version > current, in order:
        verify file exists, is readable, has the expected name shape
        execute the file's SQL statements in this transaction
        insert schema_migrations row (version, now, sha256)
      COMMIT
  → continue boot
```

All pending migrations apply in **one transaction**. Either every
new migration commits, or none do. There is no partially-migrated
state.

This places a hard constraint on migration content: **no SQL
that cannot run inside a single `BEGIN IMMEDIATE`**. SQLite's
`ALTER TABLE ... ADD COLUMN` is fine; data rewrites against
multi-million-row tables are fine (the laptop is one process,
one writer, no concurrency). The constraint that bites is
`PRAGMA journal_mode = WAL` and similar pragmas — those run at
pool open, before the migration transaction. SOLO-08 §2.1
covers the pragma sequence.

### 2.3 The pre-migration auto-snapshot

**Before applying any pending migrations, the daemon writes an
auto-snapshot.** This is not a user-initiated backup; it is a
safety net the user can ignore until they need it.

```
daemon start with pending migrations
  → write ~/.veska/backups/auto-pre-migration-<from>-to-<to>-<ts>.tar.gz
    via the same path as `veska backup create` (§4.2)
  → if the write fails: refuse to migrate; daemon exits 78 with
    ErrPreMigrationSnapshotFailed (SOLO-16). The user resolves
    disk space, then retries.
  → apply migrations as in §2.2
```

Auto-snapshot retention: keep the most recent **3** auto-snapshots
plus every snapshot in the last **30 days**. Older auto-snapshots
are deleted on the next successful migration. User-initiated
backups (`veska backup create`) are never deleted by this rule.

`veska restore --pre-migration` restores the most recent
auto-pre-migration snapshot. SOLO-13 §2 lists the verb in the
doctor adjacency.

### 2.4 Migrations and the post-promotion queue

A migration that changes columns referenced by `post_promotion_queue`
rows must drain the queue first. The order:

1. The migration file's first statement is
   `DELETE FROM post_promotion_queue WHERE state IN ('pending', 'running')`
   if and only if the migration touches a table or column the
   queue's payload references. (`embed`, `auto_link`, `revalidate`,
   `review` payloads are all keyed on `node_id` / `finding_id`
   today; a migration that renames either column drains.)
2. The pre-migration auto-snapshot captures the queue, so the
   user can re-enqueue from the snapshot if a drain was the
   wrong call.
3. After migration, `veska doctor post_promotion_queue` shows
   the drain count in `messages[]`.

Migrations that don't touch queue-referenced columns leave the
queue alone; the drainers simply pick up where they left off
after the daemon resumes.

### 2.5 Downgrade

Running an older binary against a newer schema is **refused**.
On open, if `max(schema_migrations.version) > the binary's
highest known migration number`, the daemon exits 78 with
`ErrSchemaNewer` (SOLO-16) and a remediation message naming the
binary version that wrote the schema. The user re-installs the
newer binary or restores from a pre-migration snapshot.

The check is on `max(version)`, not on file presence: an older
binary won't have the newer migration files at all. The
`schema_migrations` table is the canonical record.

There is no `veska migrate --down`. Downgrade is restore.

## 3. Upgrade during run

The user runs `brew upgrade veska` (or the equivalent on Linux).
The new binary lands on disk; the running daemon is still on the
old binary. What happens next is determined by the supervisor
(SOLO-03 §5.1) and by the connection lifecycle.

### 3.1 The two upgrade paths

| Path | Trigger | What happens |
|---|---|---|
| **Restart-driven** (default) | The package manager's post-install hook or the user runs `veska daemon restart` | Old daemon receives `SIGTERM`; drains in-flight requests up to `[shutdown].grace_seconds` (DEFAULT 10); exits clean; supervisor relaunches the new binary; new binary runs §2 migration flow. |
| **Side-by-side** (rare) | The user runs the new binary manually as `veska daemon` while the old is up | The new binary's `OpenPools` call fails: SQLite's WAL holder is the old process, and `flock(2)` on the database refuses concurrent writers. The new binary exits 78 with `ErrDaemonAlreadyRunning` (SOLO-16) pointing the user at `veska daemon stop`. |

### 3.2 The graceful shutdown contract

`SIGTERM` (or the daemon-control `Shutdown` RPC) starts the drain:

1. The MCP listener stops accepting new connections; in-flight
   requests run to completion or to `[shutdown].grace_seconds`,
   whichever comes first.
2. The promotion barrier (SOLO-11 §10.2) raises one last time;
   any in-flight promotion completes; new promotion RPCs return
   `ErrShuttingDown` (SOLO-16) so the post-commit hook knows to
   retry on the next daemon.
3. The `post_promotion_queue` drain goroutines stop pulling new
   rows. Rows in `state='running'` complete or roll back to
   `pending`.
4. The watcher unmaps fsnotify; staging is dropped.
5. The SQLite pools close. WAL is checkpointed via
   `PRAGMA wal_checkpoint(TRUNCATE)` so the new daemon doesn't
   inherit a long WAL.
6. Process exits 0.

If `[shutdown].grace_seconds` expires before step 3 completes,
the daemon exits with code 1; the supervisor restarts; the new
daemon runs the §2.4 queue-state cleanup at start.

### 3.3 What MCP clients see

- New connections during shutdown receive `ECONNREFUSED` (the
  listener is closed). Editor MCP shims reconnect automatically;
  agent clients receive a transport-level error and are expected
  to retry on the next handshake.
- In-flight requests during the drain window complete normally.
- Requests that arrive after the drain window expires (the
  pathological case) get `ErrShuttingDown`.

The editor and agent both treat `ECONNREFUSED` from the MCP
shim as a transient condition. SOLO-09 §4.6 covers the error
shape.

### 3.4 Crash-loop protection during upgrade

If the new binary crashes on start (a corrupt migration file,
an incompatible OS, a missing sqlite-vec extension), the
supervisor restarts it. The daemon's crash-loop breaker
(SOLO-03 §5.6) trips after 5 exits in 10 minutes; the
supervisor halts; `veska doctor reset-crash-loop` clears the
breaker after the user investigates.

`ErrSchemaNewer` and `ErrPreMigrationSnapshotFailed` exit with
code 78 (SOLO-16); these are *intentional* refusals and **do
not** count against the breaker.

## 4. Backup and restore

### 4.1 What gets backed up

A backup tarball contains:

```
veska-backup-<repo-host>-<ts>.tar.gz
├── manifest.json                     # version, ts, file list, hashes
├── veska.db                         # SQLite database (online snapshot)
├── veska.db-wal                     # WAL at snapshot time (may be empty)
├── audit.jsonl                       # current audit log (rotated logs not included)
├── config.toml                       # current config (secrets redacted per SOLO-13 §2.2)
└── repos.json                        # registered repos: id, root_path, active_branch
```

`veska.db` includes the `vec_nodes` virtual-table data because
sqlite-vec stores its shadow tables inside the same SQLite file
(SOLO-08 §3.3). One file means one snapshot; the backup story
is intentionally that simple.

### 4.2 The backup transaction

```
veska backup create [-o <path>]
  → daemon receives BackupCreate RPC
  → SQLite online backup API: copy veska.db page-by-page
    while readers and writers continue on the live db.
  → snapshot the current audit.jsonl (filesystem copy of the
    live file's bytes; rotations are not included)
  → write manifest.json with sha256 of every file in the tarball
  → tar+gzip atomically: write to <path>.partial, fsync, rename
    to <path>
```

The online backup runs against a hot daemon. WAL semantics
guarantee a transactionally consistent snapshot of the database
without pausing writers. The audit log copy is not transactional
relative to the database; see §4.3.

Default destination: `~/.veska/backups/veska-backup-<hostname>-<ts>.tar.gz`.
`--output <path>` overrides; the parent directory must exist.

Exit codes: 0 on success; 1 on partial (e.g., audit log read
failed but database snapshot succeeded); 2 on failure (database
snapshot failed or the tarball write failed).

### 4.3 The audit-log race

The audit log is appended-to outside the SQLite transaction, so
a backup taken during a burst of writes captures a database
snapshot at time T1 and an audit log read at time T2 > T1. Lines
T1..T2 in the audit log reference state that is in the database
snapshot. Lines after T2 reference state that **may not be** in
the snapshot.

Restoring from this backup yields a database that is internally
consistent (SQLite's online-backup contract) but whose audit log
lists a few extra lines describing writes that were never
restored. The reader contract (SOLO-08 §3.5) tolerates this: an
audit-log entry referencing a nonexistent row is treated as
informational, not corruption.

### 4.4 Restore

```
veska restore [<path> | --pre-migration | --latest]
  → if the daemon is running: refuse with ErrDaemonRunning;
    point the user at `veska daemon stop`.
  → verify manifest.json against tarball contents (sha256
    every file). Mismatch → ErrBackupCorrupt (SOLO-16) and
    abort.
  → move existing ~/.veska/veska.db, .db-wal, .db-shm to
    ~/.veska/veska.db.before-restore-<ts>.bak (idempotent
    rename; fails if the .bak already exists, prompting the
    user to clear stale rescue copies).
  → extract the tarball into ~/.veska/.
  → run integrity check: `PRAGMA integrity_check` plus a
    schema_migrations sanity check (max(version) is consistent
    with the binary's known migrations).
  → if integrity_check fails: rollback the .before-restore
    rename; abort with ErrRestoreFailed (SOLO-16).
  → on success: print summary (restored from <path>, db size,
    last_promoted_sha per repo, audit log line count).
```

`veska restore --latest` picks the newest backup tarball under
`~/.veska/backups/`. `veska restore --pre-migration` picks the
newest auto-pre-migration snapshot (§2.3).

The operation is **non-running only**. There is no live-restore
path; the daemon's pools cannot be swapped under it without
breaking in-flight transactions, and the value of speed here is
zero (restore is a recovery action, not an everyday operation).

### 4.5 Backup retention policy

`~/.veska/backups/` accumulates tarballs without bound by
default. Two configuration knobs (CONFIG-SURFACE):

| Knob | Default | Effect |
|---|---|---|
| `[backup].keep_min_count` | 3 | Always keep the N most recent user-initiated backups regardless of age. |
| `[backup].keep_max_age` | "30d" | Delete user-initiated backups older than this, subject to `keep_min_count`. |

Auto-pre-migration snapshots have their own retention (§2.3) and
are tracked separately. `veska backup prune` runs the retention
sweep; it is idempotent and does nothing on a clean dir.

### 4.6 The `veska doctor backup` surface

SOLO-13 §2.1.8 already enumerates the doctor `backup` section.
This file does not duplicate it. The relevant additions for
SOLO-17:

- `backup.required` is true if `[backup].require_before_migrate
  = true` and any auto-pre-migration snapshot is missing.
- `backup.last_pre_migration` returns the path of the most
  recent auto-pre-migration tarball; null if none.

## 5. Install and uninstall

### 5.1 What `veska init` writes

`veska init` is the user's first interaction. From a freshly
installed binary inside a Git working tree:

1. Resolve `VESKA_HOME` (DEFAULT `~/.veska`). Create if
   missing; refuse if it exists and is not a directory.
2. Verify the filesystem under `VESKA_HOME` is supported
   (SOLO-13 §2.3 allowlist).
3. Probe for Ollama (SOLO-03 §3.2). On miss, print the install
   command and exit non-zero.
4. Write the default `config.toml` if absent. Never overwrite an
   existing one.
5. Open SQLite pools, run §2 migration flow (the first run
   applies every migration from 0001 forward; the auto-snapshot
   in §2.3 is skipped on an empty database — there is nothing
   to back up).
6. Register the daemon with the session supervisor
   (SOLO-03 §5.1): `launchctl load` on macOS, `systemctl --user
   enable --now veska` on Linux with systemd-user, or write the
   `veska supervise` shim's start script otherwise.
7. Register the current Git repo via `veska repo add .`.
8. Print the summary (data dir, config path, embedder status,
   service status, registered repos, audit log path).

### 5.2 What `veska uninstall` removes

```
veska uninstall [--keep-data]
  → stop the daemon (SIGTERM, wait grace_seconds, SIGKILL on
    expiry).
  → unregister from the session supervisor (the inverse of
    step 6 above).
  → remove the supervisor's unit file or shim script.
  → if --keep-data is NOT set: prompt for confirmation, then
    delete VESKA_HOME entirely. The prompt lists the size of
    the data dir and the count of unsaved (i.e., unpromoted in
    the editor) files according to the most recent staging
    snapshot the daemon wrote on shutdown.
  → if --keep-data IS set: leave VESKA_HOME on disk; print its
    path. A future `veska init` will adopt it.
  → exit 0.
```

The binary itself is not removed by `veska uninstall` — the
package manager (Homebrew, the user's `go install` cache,
etc.) owns that. The doc notes the asymmetry.

### 5.3 Idempotency

`veska init` re-run on a populated `VESKA_HOME`:

- Detects existing config; does not overwrite.
- Detects existing schema; runs §2 migration flow if the binary
  is newer.
- Detects existing supervisor registration; updates the unit
  file's path if the binary moved (e.g. across a Homebrew
  upgrade), no-ops otherwise.
- Re-registers the current repo if not already registered.

Re-running `veska init` is the supported recovery action when
the user moves their machine, restores from a different home
dir, or otherwise needs to re-bootstrap without losing data.

## 6. Open questions

- **OQ-S017-01:** Should `veska restore` support partial
  restore (database only, audit log only, config only)? Today's
  answer is no — the tarball is one unit — but if user
  feedback in M3+ shows a real need, this is where the spec
  lands.
- **OQ-S017-02:** Should auto-pre-migration snapshots include
  a copy of the current binary so the user can downgrade
  without re-fetching? Disk cost is real; M3 measures actual
  binary sizes against typical backup sizes before deciding.

(Canonical definitions live in `design/15-glossary/open-questions.md`.)
