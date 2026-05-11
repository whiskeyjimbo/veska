---
id: SOLO-08
title: "Data & Storage — SQLite Substrate"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-04, SOLO-11, ADR-S0001]
---

# SOLO-08 — Data & Storage

## 1. The decision

Engram Solo stores everything in **one SQLite file** at
`~/.engram/engram.db`. Vector embeddings live in the same file via
the `sqlite-vec` extension. There are no child processes for
storage. There is no Dolt. There are no Dolt branches mirroring
Git branches. There is no Qdrant. There is no `_workspace`
sentinel database.

This is the largest single departure from the prior design.
Justification, alternatives, and the open question (whether SQLite
holds at 1M nodes / 5M edges on a laptop) are in
[ADR-S0001](../adr/ADR-S0001-sqlite-substrate.md).

### 1.1 sqlite-vec packaging and version pinning

sqlite-vec is a SQLite extension loaded at runtime, not a
library statically linked into the daemon. Packaging is part of
the substrate decision and is committed here rather than left to
the build system:

- **Per-platform builds.** Engram ships separate binaries for
  `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`.
  Each build embeds the matching pre-built sqlite-vec extension
  (`vec0.so` on Linux, `vec0.dylib` on macOS) as a Go `embed.FS`
  resource and writes it to `~/.engram/lib/vec0.<ext>` on first
  start (or after a binary upgrade whose embedded sqlite-vec sha
  differs from the on-disk file). The daemon loads the extension
  from that path; no external installation is required.
- **Version pinning.** The exact sqlite-vec git SHA is pinned in
  `go.mod`-adjacent build metadata; binary upgrades that bump the
  pin run as schema migrations (SOLO-08 §10) when the embedded
  vec0 schema actually changes. Patch-level sqlite-vec upgrades
  with no DDL change are hot-replaced in `~/.engram/lib/` on next
  daemon start.
- **Code signing on macOS.** The dylib is signed under the same
  developer certificate as the daemon binary. Loading an
  unsigned dylib at runtime would fail on Apple Silicon under
  default Gatekeeper rules. The release pipeline notarises both
  artifacts together.
- **Refuse-to-start on extension mismatch.** SOLO-03 §5.8 row 2
  ("sqlite-vec extension missing or unloadable") fires not only
  when the file is absent but when its sha doesn't match the
  binary's embedded sha. This catches user attempts to swap the
  extension manually.

### 1.2 mmap address-space accounting

`PRAGMA mmap_size = 268435456` (256 MiB; SOLO-08 §5.1) is set on
**every connection on every pool**. With three pools (`readDB`,
`writeDB.hot`, `writeDB.embed`) the daemon may map up to **~768
MiB of address space** for SQLite alone — independent of resident
working set, which the OS pages in on demand.

On a 16 GiB laptop running editor + browser + simulator + Slack
+ Ollama + the daemon, this is real but not pathological.
Documented here so it shows up in the resource picture instead
of being discovered. Operators with tight memory pressure can
override per-pool via the `[storage]` config (CONFIG-SURFACE).

## 2. What's in the file

```
~/.engram/
  engram.db              ← the database (SQLite + sqlite-vec)
  engram.db-wal          ← WAL file (managed by SQLite)
  engram.db-shm          ← shared-memory index (managed by SQLite)
  audit.jsonl            ← append-only audit log
  config.toml            ← daemon config
  cli.sock               ← Unix socket; CLI traffic; actor_kind=human
  mcp.sock               ← Unix socket; MCP traffic; actor_kind=agent
  daemon.pid             ← PID file
  cache/                 ← HTTP response cache (vuln feeds, etc.)
  models/                ← downloaded embedder models (if any)
```

That is the entire on-disk footprint. Reset is `rm -rf
~/.engram/`. Backup is **not** `tar` of the live tree — see §9
for the mechanism. Restore is documented there.

## 3. Schema

The schema lives in code at `internal/infrastructure/sqlite/migrations/`.
It is migrated forward only; we do not support downgrade. The
canonical version of the schema is the live `migrations/` tree;
this section is descriptive. §10 specifies the migration runner,
failure recovery, and the on-disk contract.

> **M0 measurement note (spike commit `72d6ca4`).** Branch-in-PK schema verified
> green: node p95 = 0.039 ms, edges p95 = 0.047 ms, 1.68 GiB for 28 branches × 100k
> symbols. Schema unchanged. m1.08 re-verifies on real-repo data.

### 3.1 Core tables

```sql
-- Per-database invariants. Single-row-style key/value table.
-- `engram init` populates it from the configured embedder and
-- the schema migration set. Subsequent boots compare config
-- to these values and refuse inconsistent boot (e.g. config
-- says model=X but the database was initialised against Y).
CREATE TABLE database_meta (
    key       TEXT PRIMARY KEY,
    value     TEXT NOT NULL,
    set_at    INTEGER NOT NULL                -- unix epoch microseconds
);
-- Required keys:
--   schema_version       e.g. "M1.0"            (matches the latest applied migration)
--   embedder_provider    "ollama"
--   embedder_model       e.g. "nomic-embed-text"
--   embedder_dim         "768"                  (decimal string; matches vec_nodes geometry)
--   created_at           e.g. "2026-05-09T15:42:31Z"
-- Adding a new required key is a migration that inserts the
-- value and a daemon-side check that requires it.

-- Repos the daemon knows about.
CREATE TABLE repos (
    repo_id          TEXT PRIMARY KEY,        -- stable hash of repo root path
    root_path        TEXT NOT NULL UNIQUE,
    added_at         INTEGER NOT NULL,        -- unix epoch microseconds
    active_branch    TEXT,                    -- last branch observed via post-checkout hook
    last_promoted_sha  TEXT,                    -- last commit promoted to promoted state
    module_path      TEXT                     -- e.g. "github.com/org/sdk" (Go) or npm package name; used by the cross-repo resolver (SOLO-11 §9)
);

-- Promoted graph nodes.
CREATE TABLE nodes (
    node_id        TEXT NOT NULL,        -- stable hash (repo, lang, symbol_path)
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,        -- function|type|file|package|method|field
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    content_hash   TEXT NOT NULL,        -- sha256 of node body
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX idx_nodes_symbol ON nodes(symbol_path);
CREATE INDEX idx_nodes_content_hash ON nodes(content_hash);

-- Promoted graph edges.
CREATE TABLE edges (
    edge_id        TEXT NOT NULL,        -- stable hash (src, dst, kind)
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    src_node_id    TEXT NOT NULL,
    dst_node_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,        -- CALLS|IMPORTS|CONTAINS|TESTS|DEPENDS_ON
    confidence     TEXT NOT NULL,        -- unresolved|probable|strong|definite
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (edge_id, branch),
    -- Foreign keys reference (node_id, branch) since both columns now compose
    -- the nodes PK; the application layer enforces the (src_node_id, branch)
    -- and (dst_node_id, branch) targets exist on the same branch.
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (dst_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE
);
CREATE INDEX idx_edges_src ON edges(src_node_id, branch, kind);
CREATE INDEX idx_edges_dst ON edges(dst_node_id, branch, kind);
CREATE INDEX idx_edges_repo_branch ON edges(repo_id, branch);

-- Cross-repo edge stubs (SOLO-04 §5.2.1). NOT edges: these are
-- value objects attached to a source node that record an
-- unresolved cross-repo target by (module_path, symbol_path,
-- language). The resolver chain (SOLO-11 §9) reads stubs at
-- query time and projects them into the MCP response as
-- synthetic edges. There is no FK to a target node — by
-- definition the target is not in this repo's `nodes`.
CREATE TABLE cross_repo_edge_stubs (
    stub_id        TEXT NOT NULL,        -- stable hash (src, kind, module_path, symbol_path, language)
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    src_node_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,        -- CALLS|IMPORTS|DEPENDS_ON
    module_path    TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    language       TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (stub_id, branch),
    -- Source endpoint must exist; no FK on the cross-repo target.
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_stubs_src ON cross_repo_edge_stubs(src_node_id, branch);
CREATE INDEX idx_stubs_resolver ON cross_repo_edge_stubs(language, module_path, symbol_path);
CREATE INDEX idx_stubs_repo_branch ON cross_repo_edge_stubs(repo_id, branch);
```

### 3.2 Tasks, findings, suppressions

```sql
CREATE TABLE tasks (
    task_id       TEXT PRIMARY KEY,
    repo_id       TEXT NOT NULL,         -- task is scoped to one repo
    tracker       TEXT,                  -- 'bd-cli' | 'jira' | 'github' | NULL
    tracker_ref   TEXT,                  -- e.g. 'bd-cli:engram-42'
    title         TEXT NOT NULL,
    active        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
-- One active task per repo, not one globally.
CREATE UNIQUE INDEX idx_tasks_active_one_per_repo ON tasks(repo_id) WHERE active = 1;

CREATE TABLE findings (
    -- finding_id is branch-stable: stable_hash(rule, anchor) where
    -- anchor is node_id (itself branch-stable) or file_path. The
    -- same conceptual finding on two branches shares finding_id;
    -- only the (finding_id, branch) row is per-branch. This mirrors
    -- the nodes/edges schema shape.
    finding_id    TEXT NOT NULL,
    branch        TEXT NOT NULL,
    repo_id       TEXT NOT NULL,
    node_id       TEXT,                  -- nullable: file-level findings
    file_path     TEXT,
    severity      TEXT NOT NULL,         -- info|low|medium|high|critical
    source_layer  TEXT NOT NULL,         -- structural|semantic|security|quality
    rule          TEXT NOT NULL,
    message       TEXT NOT NULL,
    state         TEXT NOT NULL,         -- open|closed
    closed_reason TEXT,                   -- free-form when state='closed'; NULL otherwise
    created_at    INTEGER NOT NULL,
    closed_at     INTEGER,
    actor_id      TEXT NOT NULL,
    actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (finding_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_findings_state ON findings(state, severity);
CREATE INDEX idx_findings_anchor ON findings(node_id, branch);
CREATE INDEX idx_findings_repo_branch ON findings(repo_id, branch);

CREATE TABLE suppressions (
    suppression_id TEXT PRIMARY KEY,
    scope          TEXT NOT NULL,        -- finding|symbol|file|repo
    target         TEXT NOT NULL,        -- the symbol_path / file_path / finding_id
    branch         TEXT,                 -- nullable: NULL = all branches; non-NULL = this branch only
    rule           TEXT,                 -- nullable: scope=finding doesn't need it
    reason         TEXT NOT NULL,
    expires_at     INTEGER,              -- nullable: permanent
    created_at     INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system'))
);
CREATE INDEX idx_suppressions_target ON suppressions(target, branch);
```

### 3.3 Embeddings (sqlite-vec)

```sql
-- Content-addressed embedding bytes.
CREATE TABLE node_embeddings (
    content_hash  TEXT PRIMARY KEY,
    model         TEXT NOT NULL,         -- e.g. 'nomic-embed-text:v1.5'
    dim           INTEGER NOT NULL,      -- 768
    embedding     BLOB NOT NULL,         -- raw float32 bytes
    created_at    INTEGER NOT NULL
);

-- Per-node refs into the content-addressed store.
CREATE TABLE node_embedding_refs (
    node_id       TEXT PRIMARY KEY,
    content_hash  TEXT,                  -- NULL while pending
    state         TEXT NOT NULL,         -- pending|ready|failed
    enqueued_at   INTEGER NOT NULL,
    embedded_at   INTEGER,
    FOREIGN KEY (node_id) REFERENCES nodes(node_id) ON DELETE CASCADE,
    FOREIGN KEY (content_hash) REFERENCES node_embeddings(content_hash)
);
CREATE INDEX idx_node_embedding_refs_state ON node_embedding_refs(state, enqueued_at);

-- The vec0 virtual table (sqlite-vec) for ANN search.
-- The dim is per-database, baked at `engram init` time from the
-- configured embedder's `ModelVersion()`. The example below shows
-- the default (nomic-embed-text, 768); a database initialised
-- against a 1536-dim model would carry FLOAT[1536] here.
CREATE VIRTUAL TABLE vec_nodes USING vec0(
    content_hash TEXT PRIMARY KEY,
    embedding    FLOAT[<dim>]
);

-- FTS5 lexical index on symbol_path and name. This is the
-- documented fallback for `eng_search_semantic` when the
-- embedder is unreachable or the affected nodes are still
-- pending embeddings (SOLO-13 §4). It is always populated;
-- the promotion pipeline writes to it inside the promotion transaction.
CREATE VIRTUAL TABLE node_fts USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    symbol_path,
    name,
    tokenize = "unicode61 remove_diacritics 2"
);
```

The FTS5 index is small relative to `nodes` (BM25 over short
identifier strings, no body content) and its write cost lands
inside the promotion transaction's per-node insert path. Lexical
fallback ranks by FTS5 BM25 over `symbol_path` and `name`; the
ranking is documented as inferior to vector recall and is
surfaced to callers via `degraded_reasons:
["embedder_offline_lexical_fallback"]` (SOLO-09 §4.5) so an
agent does not silently switch reasoning modes.

The split (`node_embeddings` content-addressed + `node_embedding_refs`
per-node) is deliberate: rename and content-equivalent moves don't
re-embed. The `vec0` table is just the ANN index over the
content-addressed bytes.

**Per-database dim.** The vec0 dimension is fixed for the life of
a database, baked at `engram init` time from the embedder's
`ModelVersion()`. `database_meta` records the provider/model/dim;
`node_embeddings.dim` per-row is the sanity check that catches
stale rows after a swap. Boot refuses on mismatch with
`ErrEmbedderMismatch`. Swaps go through `engram embedder swap`
(SOLO-03 §3.2) — the canonical mechanism.

### 3.3a Daemon state

```sql
-- Single-row-style key/value table for daemon-runtime state that
-- must survive process restart but is not part of the domain
-- model. The crash-loop breaker counter (SOLO-03 §5.6) lives
-- here; future entries (last-clean-shutdown timestamp, etc.)
-- extend the same table rather than spawning new files.
CREATE TABLE daemon_state (
    key       TEXT PRIMARY KEY,
    value     TEXT NOT NULL,
    set_at    INTEGER NOT NULL                -- unix epoch microseconds
);
-- Required keys at first boot:
--   restart_count_window_start  unix epoch microseconds
--   restart_count               integer (decimal string)
-- Updates run inside the breaker's own short transaction on
-- writeDB.hot before any other goroutine starts. SQLite handles
-- atomicity; no flock dance. Recovery if engram.db itself is
-- corrupt: the daemon refuses to start at the migration runner
-- (SOLO-08 §10) and the breaker is moot — there's no daemon to
-- restart-loop.
```

### 3.4 Post-promotion queue

```sql
CREATE TABLE post_promotion_queue (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    promotion_id       TEXT NOT NULL,         -- ULID per commit
    repo_id       TEXT NOT NULL,
    branch        TEXT NOT NULL,
    git_sha       TEXT NOT NULL,
    work_kind     TEXT NOT NULL,         -- embed|auto_link|revalidate|review
    payload       TEXT NOT NULL,         -- JSON
    state         TEXT NOT NULL,         -- pending|deferred|in_progress|done|failed
    attempts      INTEGER NOT NULL DEFAULT 0,
    enqueued_at   INTEGER NOT NULL,
    completed_at  INTEGER,
    error         TEXT
);
CREATE INDEX idx_post_promotion_queue_state ON post_promotion_queue(state, work_kind, seq);
```

One table, four work-kinds, no per-lane sub-tables. Goroutines poll
the table at 250ms (reads via `readDB`); one goroutine per
`work_kind`. State transitions (`pending → in_progress → done`)
write via `writeDB.hot`; payload work for `embed` writes via
`writeDB.embed` (SOLO-11 §10.3, ADR-S0011). When `state = failed`
after 3 retries, the row is logged and **stays `failed`**. There
is no 24h auto-acknowledge sweeper. Failure handling is per-
`work_kind` (full policy in ADR-S0004):

- `embed`: row stays `failed`. Reads against affected nodes
  surface `degraded_reasons:["embedding_failed"]` (distinct from
  `embedding_pending` — the work is not in flight, it has given
  up). The user retries via `engram doctor post-promotion-queue retry --kind=embed [--seq=N]`,
  which moves matching `failed` rows back to `pending` and
  resets `attempts` to 0. There is no automatic retry; a model
  pull or config fix is the typical resolution and the user is
  the one doing it.
- `auto_link`: row stays `failed`. Subsequent commits re-run
  auto-link over the affected nodes, so the failure is
  self-healing in normal use; the doctor surface lists the
  failed seq IDs for diagnosis.
- `revalidate`: row stays `failed`. The hourly revalidation
  sweep (SOLO-11 §6) picks up open findings on its own cadence,
  so a missed post-promotion queue row does not silently lose state. The
  failed row is for diagnosis.
- `review`: **sticky** with a finding — a `Finding` is emitted
  (`rule='review-pipeline-failure'`, `severity='high'`); the
  post-promotion queue row stays `failed` until the finding closes through the
  human-action gate. Closing the finding flips the row to `done`.

`engram doctor post-promotion-queue` lists every `failed` row with its seq,
work_kind, repo, branch, attempts, and last error. The user's
retry path is uniform: `engram doctor post-promotion-queue retry [--kind=K] [--seq=N]`.
Without flags it retries every `failed` row across all kinds.

Queue-depth signalling: `post_promotion_queue.high_water` (default 10 000) and
`post_promotion_queue.low_water` (default 8 000) are *signal thresholds*, not
promotion blockers. When the queue exceeds high-water, the promotion
transaction:

1. Inserts the row with `state = 'deferred'` instead of
   `'pending'` for `work_kind` rows that are not on the
   user-blocking path. `embed` is the only deferrable kind today;
   `auto_link`, `revalidate`, and `review` are not deferred —
   they queue normally and the user-blocking path is the promotion
   itself, which proceeds.
2. Emits `degraded_reasons: ["post_promotion_queue_deferred:embed:<count>"]`
   on subsequent MCP reads until depth drops below low-water.
3. Surfaces the deferred count in `engram doctor post-promotion-queue`.

Deferred rows transition to `pending` as soon as queue depth
drops below `low_water`; the embed worker picks them up on its
normal cadence. **Promotion never blocks on post-promotion queue depth.** This
removes the prior deadlock where embedder pause (RSS pressure)
plus high-water would deadlock the promotion — the user's commit is
durable; embedding is async best-effort.

Nothing is silently dropped: `deferred` is a real state, visible
to the doctor surface and counted in audit lines for the promotion
that produced the deferral.

**Sustained-degradation finding.** A `degraded_reasons` string
on every MCP read is enough signal for transient bursts but
not for sustained pressure: under continuous refactor load on
a CPU-bound Ollama, embed deferral can persist for hours or
days, during which `semantic_search` returns answers that miss
recently-changed code. To make sustained degradation a visible
artifact rather than a quietly-degraded background condition,
the daemon emits a sticky finding `embed-deferred-saturated`
(rule `embed-deferred-saturated`, severity `medium`,
`source_layer = quality`) when the deferred-embed queue's
oldest row's age exceeds `post_promotion_queue.deferred_age_threshold`
(default `24h`, CONFIG-SURFACE). The finding clears
automatically when the deferred queue empties; it is not
human-action-gated. This is the same pattern as
`review-pipeline-failure` (SOLO-11 §2.2) — a sticky finding
gives an editor surface for a condition that the
`degraded_reasons` line alone makes too easy to ignore. The
finding's payload carries the deferred-row count and the
oldest row's age so the user can decide whether to wait,
swap embedder, or pause indexing on selected repos.

`engram doctor post-promotion-queue` surfaces row counts by state and `work_kind`,
and lists the seq IDs of any `failed` row aged past its policy.

### 3.5 Audit log shape

`audit.jsonl` is **not** a table — it's an append-only file. The
**writer rule** (when the daemon appends a line) lives in SOLO-10
§4. This section is the **wire shape and stability contract**.
One JSON object per line.

#### Required fields (`v: 1`)

| Field | Type | Notes |
|---|---|---|
| `v` | integer | Schema version. Currently `1`. See §3.5.3 for the bump rule. |
| `ts` | RFC 3339 string, UTC, microsecond precision | Server-side; never client-supplied. |
| `actor_id` | string | Per SOLO-10 §1.1. |
| `actor_kind` | `"human" \| "agent" \| "system"` | Per SOLO-10 §1.2. |
| `tool` | string | MCP tool name (`eng_*`) for MCP writes; `cli:<verb>` for CLI invocations; `service:<routine>` for daemon-internal writes. |
| `args` | object | Tool arguments. Secret-shaped string values redacted at write time: keys matching `*_token`/`*_secret`/`*_password`/`*_key`/`api_key`/`auth*` (case-insensitive) are replaced with `"<redacted>"`, and **every string value is scanned by the SecretsScanner port** (the same scanner promotion uses, SOLO-11 §2.1); content matches are replaced with `"<redacted:<rule>>"`. The scan is finite-rule and doesn't catch novel formats; the audit log is operator data, not sanitised data. |
| `result` | string | `"ok"`, or `"refused: <reason>"` for human-action-gate / validation refusals, or `"error: <engram_code>"` for handler errors. |

#### Optional fields

| Field | Set when |
|---|---|
| `resolver` | Tool's answer materialised cross-repo synthetic edges (SOLO-04 §5.4, SOLO-11 §9). Records the resolved outputs, not the inputs — see §3.5.1. |

#### 3.5.1 Resolver record (cross-repo reads)

```json
{
  "v": 1,
  "ts": "2026-05-08T15:42:31.123456Z",
  "actor_id": "agent:claude-code",
  "actor_kind": "agent",
  "tool": "eng_get_call_chain",
  "args": {"node_id": "n_...", "depth": 3},
  "result": "ok",
  "resolver": {
    "edges": [
      {
        "src_node_id":          "n_local_...",
        "src_branch":           "main",
        "target_repo_id":       "r_sdk_...",
        "target_active_branch": "main",
        "target_node_id":       "n_remote_...",
        "via":                  "module_path"
      }
    ]
  }
}
```

Recording inputs alone is not enough: the resolver's output
depends on the target's `active_branch` at query time, which can
change between calls. Without `target_node_id` +
`target_active_branch`, post-hoc audit cannot reproduce the edge
once the target promoted or switched branches.

The `resolver` field is omitted when the tool's answer used no
cross-repo materialisation.

#### 3.5.2 Reader contract

- **Tolerate unknown fields.** Readers that don't recognise a
  field MUST ignore it (forward compat).
- **Tolerate unknown `tool` and `result` values.** Both are
  open vocabularies; a new tool name or refusal reason does not
  bump `v`.
- **Trust `v`.** A reader written for `v: 1` MAY refuse to parse
  lines with `v >= 2` rather than guess at semantics.

#### 3.5.3 Stability and the bump rule

`v` is a small monotonic integer. Bumps follow the SOLO-13 §2.1
discipline applied to the audit shape:

| Change | Bumps `v`? |
|---|---|
| Adding a new optional field | no |
| Adding a new `tool` name or `result` string | no |
| Adding a new optional sub-field inside `resolver.edges[]` | no |
| Removing or renaming a required field | yes |
| Changing the type of an existing field | yes |
| Repurposing a field's meaning | yes |

A `v` bump ships only in a minor Engram version with a migration
note in CHANGELOG. Patch releases never bump `v`.

#### 3.5.4 Rotation

When the file exceeds 100MB, it rotates to `audit.jsonl.1` (and
so on, keeping 5). Each rotation file contains lines from one or
more `v` values if a bump landed mid-file (rare; bumps coincide
with binary upgrades, which restart the daemon and start a fresh
file in practice).

No streaming target. No SIEM webhook. No Splunk HEC. If you want
any of those, `tail -F` the file and pipe it.

## 4. Branches: part of the primary key

There is no Dolt-branch-per-Git-branch. `branch` is part of the
composite primary key on `nodes` and `edges`, so each branch
keeps its own row for every symbol it touches. Switching branches
in Git triggers a tree-sitter rescan into staging; on promotion, rows
are written under the new branch's PK.

This means each branch carries the cost of the symbols it has
promoted, not the union of all branches. Two branches that share
content of `Foo` carry **two `nodes` rows** (one per branch); the
shared content is deduplicated only in the embedding tables
(`node_embeddings` is content-addressed; `vec_nodes` keys on
`content_hash`). Embeddings cost stays roughly flat across
branches; nodes/edges scale with `(symbols_touched × branches)`.

The actual storage cost for a multi-branch population is unknown
until measured. **OQ-S006** gates this at the **M0 spike** (epic
`m0.02-branch-pk-spike`, synthetic schema loader, run before any
M1 code is written): measure node/edge/finding row counts and
disk size at 50+ branches with configurable per-branch overlap.
M1's m1.08 re-verifies the same shape on real-repo data at M1
close.

**Trip condition.** Per-branch row growth is compared against
`[branch_pk].max_growth_per_branch_pct` (DEFAULT 10% of trunk row
count per active branch; CONFIG-SURFACE). If the measured growth
exceeds the threshold, the M0 close report trips
**ADR-S0013** (branch-PK delta scheme; status: proposed,
pre-authored as the fallback storage layout) from *proposed* to
*accepted*, and M1's m1.03 schema work targets the trunk+delta
shape from the first migration. If the measurement is under
threshold, ADR-S0013 retires (withdrawn) and this section
stands.

The pre-authored fallback removes the prior risk that a red
spike would force the team to invent the alternate storage
shape under M1 deadline pressure.

Branch GC runs at daemon start (after resync) and after every
wake-reconcile sweep (SOLO-03 §5.2) — never on a wall-clock cron,
because laptops sleep through wall clocks. `engram gc --branches`
is the manual handle for the same routine. It sweeps rows whose
`branch` value no longer appears in `git branch -a`. Default
retention: 7 days post-deletion. With branch-in-PK this sweep is
load-bearing — it is the primary defence against unbounded growth.

### 4.1 Findings and the branch question

`findings` PK is `(finding_id, branch)` — see §3.2. The decision
mirrors `nodes` / `edges`: per-branch state is required to
correctly represent findings whose rule premise is per-branch
(dead-code, contract-drift, anything anchored to per-branch
content). Cross-branch behaviour for findings that *should*
persist across branches (vulns, secret leaks) is handled by the
`Suppression` model's `branch` column (NULL = all branches; SOLO-04
§8.2, SOLO-11 §5.2), not by collapsing the PK.

`finding_id` itself is branch-stable
(`stable_hash(rule, anchor)` where `anchor` is `node_id` or
`file_path`, both of which are themselves branch-stable). The
same conceptual finding shares `finding_id` across branches; only
the per-branch row carries per-branch state. A suppression
referencing `finding_id` matches all per-branch rows by default
(branch-agnostic); a suppression referencing `(finding_id, branch)`
matches one branch.

The cost question (per-branch row growth) is part of OQ-S006's
M0 measurement — the same audit that covers nodes/edges row
growth, re-verified on real-repo data at M1 close.

## 5. The promotion transaction

One SQL transaction per commit:

```
BEGIN IMMEDIATE;
  -- promote staging nodes/edges
  INSERT INTO nodes (...) VALUES (...);
  INSERT INTO edges (...) VALUES (...);
  -- enqueue async work
  INSERT INTO post_promotion_queue (work_kind, payload, ...) VALUES ('embed', ...);
  INSERT INTO post_promotion_queue (work_kind, payload, ...) VALUES ('auto_link', ...);
COMMIT;
```

All hot-path writes — the promotion transaction and MCP write tools —
go through the **`writeDB.hot`** pool (SOLO-11 §10, ADR-S0011),
which is a `*sql.DB` opened with `MaxOpenConns=1`. The pool is
the queue; SQLite's writer lock arbitrates against the embed
pool (`writeDB.embed`) under `BEGIN IMMEDIATE`. In steady state
the promotion transaction's `BEGIN IMMEDIATE` succeeds without retry
because the only other writer pool is the embed worker, which
holds the lock for bounded chunks. `PRAGMA busy_timeout = 5000`
on the hot pool's connections covers worst-case contention; we
do not implement application-level backoff. The transaction is
bounded by the staging delta size, typically a handful of files;
a 50k-symbol refactor is the worst case. Budget in SOLO-13 §3.2
("Promotion SQL transaction"); unmeasured at write time, gated by M1.

### 5.1 PRAGMAs

Every connection on every pool runs the same PRAGMA bundle on
open (the helper `infrastructure/sqlite/open.go` enforces this):

```
PRAGMA journal_mode    = WAL;
PRAGMA synchronous     = FULL;
PRAGMA foreign_keys    = ON;
PRAGMA busy_timeout    = <per-pool>;   -- 5000 hot/read, 30000 embed
PRAGMA wal_autocheckpoint = 1000;      -- pages
PRAGMA temp_store      = MEMORY;
PRAGMA mmap_size       = 268435456;    -- 256 MiB; advisory, OS may cap
```

WAL mode means readers (`readDB`) never block on either writer
pool, and the promotion transaction (`writeDB.hot`) runs concurrently
with embed work (`writeDB.embed`) under SQLite's lock.

**`synchronous = FULL` on every platform.** The cost is one
`fsync` per `COMMIT`. Real-world cost on commodity hardware:
Linux ext4/btrfs over consumer NVMe is 1–10 ms; macOS
`F_FULLFSYNC` is 5–50 ms because Apple disks honour the cache
flush. The high end of the macOS band is a meaningful slice of
SOLO-13 §3.1's typical-commit budget; we eat the cost.

**Why uniform FULL** (the prior platform-split default was
retired after the principal-architect review of 2026-05-09):
the macOS-NORMAL story relied on the startup-resync path
(SOLO-03 §5.7) replaying missing promotions from Git on next
start. That defence only holds if Git itself fsync'd the HEAD
update. Git's `core.fsync` defaults vary by version and distro;
on a real power-cut both Engram's promotion and Git's HEAD
advance can be lost independently, after which
`last_promoted_sha == HEAD` looks consistent and resync sees
nothing to do — but the user's commit work is gone. Honest
durability everywhere is worth the budget hit. SOLO-13 §3.1's
macOS row carries the cost.

Operators who explicitly want the lower-durability NORMAL mode
on macOS (or any platform) set `[storage].synchronous = "NORMAL"`
in `~/.engram/config.toml`. The config is honoured; the doc
defaults are FULL. We do not offer `OFF`.

OQ-S002 (M1) measures actual fsync cost on the reference laptop
to confirm the budget rows stay green under refactor load.

The startup resync (SOLO-03 §5.7) still runs unconditionally —
the daemon may be down across user-side commits, in which case
`last_promoted_sha < HEAD` is normal and the replay catches up.

## 6. Failure modes

| Failure | Behavior |
|---|---|
| Daemon crashes mid-promotion | `BEGIN IMMEDIATE` was either committed (Git already has SHA → next commit succeeds) or rolled back (next commit retries). Neither leaves partial state. |
| Power loss / kernel panic between promotion `COMMIT` and next checkpoint | With `synchronous = FULL` (default) the promotion is durable. Operators who overrode to `NORMAL` may lose the most recent promotion; startup resync (SOLO-03 §5.7) replays when `last_promoted_sha < HEAD`. |
| `last_promoted_sha` not reachable from `HEAD` (force-push, history rewrite) | Startup resync logs `ErrPromotionDivergent`, falls back to a fresh full reparse for the active branch, and records the new `HEAD` as `last_promoted_sha`. Findings on the orphaned branch persist until `engram gc --branches` sweeps them. (SOLO-03 §5.7.) |
| SQLite database locked by an external writer (e.g. user `sqlite3` shell) | `BEGIN IMMEDIATE` blocks until `busy_timeout` expires (5s hot, 30s embed); the in-flight op then fails. User can `engram promote --retry` after closing the external connection. In-process pools do not contend with each other beyond SQLite's own lock arbitration (SOLO-11 §10, ADR-S0011). |
| Embedding goroutine crashes | Daemon supervisor restarts it; pending refs stay `pending`; semantic search returns `degraded_reasons: ['embedding_pending']`. |
| Disk full | Promotion fails; hook returns non-zero; daemon refuses new MCP writes until `engram doctor disk` reports OK. The daemon also (a) checkpoints WAL aggressively (`PRAGMA wal_checkpoint(TRUNCATE)`) to release any pending pages, (b) rotates `audit.jsonl` and the daemon log to their retained-set sizes, and (c) refuses to write new pre-migration auto-snapshots until pressure clears (an old pre-migration snapshot is preserved; a new one would have nowhere to land). Backups are not auto-pruned — the user owns retention there (§9.5). |
| WAL grows unboundedly | Daemon checkpoints WAL on idle (no writes for 5s). `PRAGMA wal_autocheckpoint = 1000` (pages). |
| sqlite-vec extension missing | Daemon fails to start with a clear error pointing at install instructions. No silent degradation. |

### 6.1 Cross-repo edges are not in `edges`

No row in `edges` is ever cross-repo. Invariant 1 of SOLO-04
§5.2 is absolute: every stored `Edge` has both endpoints in
`nodes` (enforced by `dst_node_id NOT NULL` and the composite
FK). Cross-repo answers are computed at query time by the
resolver chain (SOLO-11 §9): when an edge's target is
unresolved within its own repo, the resolver matches `(language,
symbol_path)` against other indexed repos using
`repos.module_path`. The MCP response tags synthetic edges
(`cross_repo: true, target_repo_id: ..., target_branch: ...`).

The one stored thing is the **`CrossRepoStub`** (SOLO-04
§5.2.1) — a value object, **not an `Edge`** — held in the
sibling `cross_repo_edge_stubs` table defined in §3.1. A stub
is written at source-side promotion when a parser produces a
cross-repo reference whose target repo is not yet indexed (or
whose target symbol has not yet promoted on its active branch).
The stub records `(src_node_id, kind, module_path,
symbol_path, language)`; it has no `confidence` and no
`dst_node_id` (by construction the target is not in this
repo's `nodes`). The resolver projects stubs into MCP
responses at read time as synthetic edges with
`confidence: "unresolved"` and `cross_repo_target` populated;
stubs are never rewritten in place, and they are GC'd via the
`ON DELETE CASCADE` from `nodes(src_node_id, branch)` when the
source file is deleted or the source repo is forgotten.

Tradeoff: cross-repo answers reflect each target repo's promoted
state *as captured at query start* (SOLO-04 §5.4 invariant 2).
The per-query `as_of: [{repo_id, branch, promoted_sha}, ...]`
envelope makes this auditable. If query-time resolution misses
its budget (SOLO-13 §3.4), the fallback is a query-time
materialisation cache invalidated on target-repo promotion — not a
promotion of stubs into `edges`. The `Edge` aggregate stays
pure; cross-repo bookkeeping stays out of it.

## 7. What this design does NOT include

- **Embedder migration ceremony.** Swapping models is one CLI
  subcommand against a live daemon (`engram embedder swap`) —
  see SOLO-03 §3.2 and ADR-S0007. There is no five-phase FSM,
  no per-row migration cursor, no dual-index window.
- **Cross-database joins.** There is one database. There are no
  joins to make.
- **Replication.** None.
- **Snapshot reads.** SQLite WAL gives readers a consistent view
  for the duration of a transaction; that is the only "snapshot"
  we offer. No `Graph.Snapshot()` API.
- **Reachability sweeps comparing Dolt vs. Git.** N/A — no Dolt.

## 8. Open questions

- **OQ-S001:** Does sqlite-vec hold its `< 100ms p95 k=10` budget
  at 50k vectors on the M1 reference laptop? **Resolved (M0).** 50k p95 = 80.98 ms (PASS ≤ 100 ms); recall@10 = 1.0000 (PASS ≥ 0.95). Spike commit `4d63d34`.
- **OQ-S002:** WAL checkpoint cadence under sustained refactor
  storms (50k symbols/commit, repeated). **Resolution:** M1 spike.
- **OQ-S003:** Does `vec0`'s Hamming/cosine ANN remain accurate
  enough for code-search recall at 1M-node scale? Or do we need
  HNSW via lancedb at that point? **Resolution: mandatory M1 work before m1.03.** M0 measured vec0 ceiling at 100k nodes (below the 250k minimum). The HNSW pivot ADR is not optional — it must be written and accepted before m1.03 begins. See ADR-S0001 §M0 Measurement.
- **OQ-S006:** Branch-in-PK row growth and GC cost at 50-branch scale.
  **Resolved (M0).** Spike commit `72d6ca4` (28 branches × 100k symbols, 10% dirty overlap):
  node p95 = 0.039 ms (PASS ≤ 25 ms); edges p95 = 0.047 ms (PASS ≤ 100 ms);
  disk = 1.68 GiB for 28 branches (linear growth; 50-branch extrapolation ≈ 3.0 GiB,
  well under the 5 GiB green threshold). GC sweep deleted 10 of 28 branches in ~519 s
  with 700 MiB reclaimed (bounded). **Verdict: green.** Schema in §3/§4 stands.
  m1.08 re-verifies on real-repo data.

These are real questions, not handwaves. Each has a milestone
gate.

## 9. Backup, restore, integrity

`tar` of `~/.engram/` while the daemon is running is **unsafe**.
The triple `engram.db` + `engram.db-wal` + `engram.db-shm` is in
flux during writes; an archive captured mid-checkpoint can pass
`PRAGMA integrity_check` after restore and still corrupt
downstream behaviour. We do not document `tar` as the backup
mechanism.

### 9.1 Mechanism: `VACUUM INTO`

**Wall-clock cost first.** `VACUUM INTO` on a 5 GiB database can
take tens of seconds to several minutes depending on page
fragmentation and concurrent write pressure. The daemon stays
responsive throughout (WAL keeps reads off the writer), but a
backup overlapping a refactor commit visibly slows the refactor.
The pre-migration auto-snapshot path (§10) inherits this cost:
upgrading the daemon binary against a 5 GiB DB may block start
for 60+ seconds before the migration runner begins. The
`engram upgrade` flow surfaces "snapshot in progress" through
the shim's `cli_command` payload so the editor doesn't appear
hung. M1 measures actual wall-clock against representative DBs.

Backup is a SQLite **online snapshot** to a single self-contained
file:

```sql
VACUUM INTO '<dest_path>';
```

Properties that matter:

- Runs against the live database with WAL mode on; readers and
  the promotion transaction proceed concurrently. The snapshot reads
  a consistent point-in-time view; subsequent writes don't touch
  the destination file.
- Produces **one file** — no `-wal` / `-shm` siblings — that
  passes `PRAGMA integrity_check` by construction.
- The destination file is smaller than the source (`VACUUM`
  defragments the page layout while it copies).
- Cost is bounded by source size, not transaction history. The
  daemon does not need to be quiesced.

**Mechanically:** `VACUUM INTO` is a random-read on the source
pages plus a sequential write to the destination — not a single
sequential I/O pass. `engram doctor backup` surfaces the age of
the last successful backup so the user can see drift; we do not
guarantee a daily cadence on laptops that suspend or are powered
off (see §9.5).

Audit log, config, and cache files are plain files; they are
copied alongside the snapshot in a tarball.

### 9.2 `engram backup create`

```
engram backup create [--dest <path>]
```

Default destination: `backup.default_path` from CONFIG-SURFACE.md
(default `~/.engram-backups/`), filename
`engram-YYYYMMDD-HHMMSS.tgz`.

Steps:

1. Build a temp directory `~/.engram-backups/.staging/<ts>/`.
2. Run `VACUUM INTO '.staging/<ts>/engram.db'` against the live
   daemon DB. The daemon executes this; the CLI sends an RPC.
3. Copy `config.toml`, `audit.jsonl` (and rotated siblings),
   `cache/` into the staging dir. Audit and cache are append-only
   or content-addressed; a plain copy is consistent.
4. `tar -czf <dest>` over the staging dir.
5. Run `engram backup verify <dest>` (§9.3) before reporting
   success.
6. Remove the staging dir.

The `cli.sock`, `mcp.sock`, and `daemon.pid` are **not** backed
up — they are runtime sentinels recreated at start. `models/`
is **not** backed up either; the embedder fetches them on demand.

### 9.3 `engram backup verify`

```
engram backup verify <path>
```

Steps:

1. Open the tarball without extracting permanent files.
2. Stream-extract `engram.db` to a fresh temp directory.
3. Open it with SQLite, run `PRAGMA integrity_check;`.
4. Run `PRAGMA foreign_key_check;` (catches FK violations the
   integrity check doesn't).
5. Verify `audit.jsonl` is well-formed JSONL (each line parses).
6. Exit 0 if all pass, 2 on any failure (per SOLO-13 §2 exit
   codes for `engram doctor backup`).

`engram doctor backup` runs the same checks against the most
recent file in `backup.default_path` and surfaces age and
verification result.

### 9.4 Restore

```
engram backup restore <backup.tgz>
```

`engram backup restore` is a first-class command — typing the
underlying steps by hand into `~/.engram/` is the kind of
operation that ends with "I restored over my live db." The
command:

1. Refuses to run while the daemon is up. Stderr names
   `engram daemon stop` as the prerequisite. (No automatic
   stop — restore is destructive enough that we want the user
   to take that step deliberately.)
2. Verifies the tarball with the same checks as
   `engram backup verify` (§9.3) — integrity, foreign keys,
   audit JSONL well-formedness — *before* touching `~/.engram/`.
   A failed verification leaves the existing data untouched.
3. Renames the existing `engram.db`, `engram.db-wal`,
   `engram.db-shm`, `audit.jsonl`, and `cache/` to a sibling
   `~/.engram/.replaced-<ts>/` directory. Nothing is deleted.
4. Extracts the tarball into `~/.engram/`.
5. **Audit-log merge.** Appends the lines from
   `.replaced-<ts>/audit.jsonl` (post-backup-cutoff lines) onto
   the freshly-restored `audit.jsonl`, sorted by `ts`. A line
   noting the restore is appended last
   (`tool: "service:backup-restore", args: {tarball: "...",
   backup_ts: "...", merged_post_backup_lines: <n>}`). This
   preserves the agent-attribution chain across restore — losing
   it silently would be a compliance footgun.
6. Prints the path of the `.replaced-<ts>/` directory and the
   command to delete it once the user has confirmed the restore
   is good (`rm -rf ~/.engram/.replaced-<ts>`).
7. Exits 0 on success, 2 on verification failure (no changes made),
   3 on partial restore failure (changes rolled back from the
   `.replaced-<ts>/` sidecar).

If the backup was taken before a schema migration that has since
run on the destination, the daemon refuses to start (forward-only
migrations, SOLO-03 §5.5). The escape is to roll back to the
binary version that matches the schema, or restore an older
backup. Both routes leave the `.replaced-<ts>/` sidecar intact
for emergency recovery.

The audit log in the backup is merged with post-backup lines
from the sidecar (step 5) so the full chain is preserved
automatically.

### 9.5 What this design does NOT include

- **Continuous replication.** None. Use the OS's filesystem
  snapshot tooling (APFS, ZFS, Btrfs) if you want time-machine
  semantics; that is out of Engram's surface.
- **Differential / incremental backup.** Each `backup create`
  is a full snapshot. **Opt-in auto-backup with 7 retained** is
  the default shape (`[backup].auto = true` ships *off* by
  default until the cadence story is validated;
  `[backup].auto_retain = 7`). When enabled, the daemon attempts
  `engram backup create` at the first observed idle window after
  local midnight — but a laptop that is suspended, off, or
  continuously busy at midnight will simply skip days. The doc
  does not promise a guaranteed daily cadence on developer
  laptops; `engram doctor backup` reports last-successful-backup
  age so drift is visible. Auto-managed files prune past
  `auto_retain`; user-initiated backups are never auto-pruned.
  Solo developers who want no auto-backups leave
  `[backup].auto = false`.
- **Encrypted backups.** The tarball is plain. Wrap it with `gpg`
  or filesystem encryption if you need confidentiality. Engram
  redacts secret patterns at write time (SOLO-08 §3.5,
  SECRETS-RUNBOOK.md), but the audit log can still contain
  attribution and tool names.
- **Cloud upload.** `engram backup create` writes to a local
  path. `s3 cp`, `rclone`, etc. are the user's tools.

## 10. Schema migrations: forward-only, transactional, snapshot-guarded

The migration runner is the only path that changes schema. It
runs at daemon start (SOLO-03 §5.1) before any other goroutine.
Three guarantees:

1. **Each migration is one SQL transaction.** SQLite's DDL
   (CREATE TABLE / INDEX / VIRTUAL TABLE, ALTER TABLE) is
   transactional in modern releases; the runner wraps each
   migration in `BEGIN; ... COMMIT;`. A failure mid-migration
   rolls back atomically; the on-disk schema version is
   unchanged.
2. **An auto-snapshot is taken before any migration runs.** The
   runner calls `engram backup create` (SOLO-08 §9) into
   `~/.engram-backups/.pre-migration/<from-version>-<to-version>-<ts>.tgz`
   before applying the first pending migration. The snapshot is
   verified before the migration starts. If the snapshot or its
   verification fails, the migration does not run.
3. **The binary refuses to start on schema mismatch.** Each
   binary embeds `min_schema` and `max_schema` constants. On
   startup the daemon reads `schema_migrations.current` (§10.1)
   and refuses to start if it falls outside the binary's
   supported range, with the recovery commands printed to stderr.

### 10.1 Metadata table

```sql
CREATE TABLE schema_migrations (
    version       INTEGER PRIMARY KEY,        -- monotonic; e.g. 0001, 0002
    applied_at    INTEGER NOT NULL,           -- unix epoch microseconds
    binary_sha    TEXT NOT NULL,              -- the daemon binary that ran the migration
    migration_sha TEXT NOT NULL,              -- sha256 of the migration file at apply time
    applied_by    TEXT NOT NULL               -- "service:engram-daemon" usually
);
```

`schema_migrations.current` (a view) returns the highest
`version` in the table. Each row is appended in the same
transaction as the migration's DDL, so a rolled-back migration
leaves no row.

The `migration_sha` is recorded so the daemon can detect
**after-the-fact migration tampering**: if a migration file in
the binary differs from the recorded sha for the same version,
the daemon refuses to start. This catches "I edited an old
migration file in source; production has the unedited version
applied" — a class of footgun that costs days when it lands.

**Sha-calculation rule.** `migration_sha` is the SHA-256 of the
migration's **SQL text only**, byte-for-byte, with line endings
normalised to `\n` and a single trailing newline. It is
deliberately not the sha of the binary's wrapper structure
(file header, generated comment block, build timestamp), so
recompiling the same source produces the same sha. The runner
strips a leading UTF-8 BOM if present. This is the contract;
binary builders MUST hash the migration file the same way the
runner hashes it at apply time.

### 10.2 Behaviour matrix

> **Cross-ref.** SOLO-03 §5.8 is the canonical refuse-to-start
> matrix. The rows below are the migration-specific subset, all
> of which exit 78 (terminal, no breaker increment).

| Situation | Action |
|---|---|
| `current = max_schema` | Normal start. |
| `current < min_schema` | Refuse start. Stderr: "binary requires schema ≥ M, on-disk schema is N. Downgrade the binary to a version with min_schema ≤ N, or restore from a newer backup." Exit 78 (matches the crash-loop breaker exit code; the supervisor stops retrying). |
| `min_schema ≤ current < max_schema` | Run pending migrations N+1 .. max_schema, in order, each in its own transaction, with the auto-snapshot taken before the first one. |
| `current > max_schema` | Refuse start. Stderr: "binary requires schema ≤ M, on-disk schema is N. Upgrade the binary, or restore from a backup taken before the upgrade that produced schema N." Exit 78. |
| Migration N fails (constraint, syntax, sqlite-vec issue) | Transaction rolls back; on-disk schema unchanged at N-1. Daemon refuses start with stderr: "migration N failed: <error>; on-disk schema is N-1; the pre-migration snapshot at <path> is verified and ready for restore. Either fix the migration and restart, or downgrade the binary." Exit 78. |
| Daemon killed mid-migration (SIGKILL, power loss) | SQLite WAL replay returns the database to its pre-migration state on next open. Treated identically to "migration N failed" above. |
| Auto-snapshot fails before migration | Refuse start without running the migration. Stderr names the snapshot error and the recovery (free disk, fix permissions). Exit 78. |
| `migration_sha` recorded for an applied version differs from the binary's embedded migration file | Refuse start. Stderr: "migration M was applied with sha <recorded>, binary embeds sha <embedded>. The applied migration may have been edited after the fact; investigate before continuing." Exit 78. |

### 10.3 What this design does NOT include

- **Down migrations.** No `down.sql`, no `engram migrate down`.
  Recovery from a bad migration is one command:
  `engram restore --pre-migration` restores the most recent
  pre-migration snapshot from `~/.engram-backups/.pre-migration/`
  (the snapshot the runner took before the failing migration in
  §10's step 2), prints which version was rolled back to, and
  prints the binary version that pairs with that schema so the
  user can downgrade. The user runs `brew downgrade engram` (or
  the equivalent) and restarts. **This is not auto-restore** —
  auto-restore on a failing migration would mask migration bugs;
  the user issues the explicit restore once they have read the
  failure message.
- **Online migrations.** All migrations run at daemon start with
  the daemon stopped. There is no zero-downtime story; this is
  a single-user product and "stopped for 200ms while migrations
  run" is the entire downtime story.
- **Skip / squash.** Migrations always run in order; the runner
  does not consolidate or skip applied versions.
- **Out-of-band migration.** `engram migrate` as a CLI surface
  does not exist. Migrations are triggered by daemon start.
  Manual SQL against `engram.db` while the daemon is stopped is
  supported but unsupported (see SOLO-11 §10.4 "What this design does NOT include" — the user is on
  their own).

### 10.4 The auto-snapshot retention policy

Pre-migration snapshots accumulate in
`~/.engram-backups/.pre-migration/`. Default retention: keep the
last 5; delete older. Configurable via `[backup].pre_migration_keep`
(DEFAULT 5; CONFIG-SURFACE.md). `engram doctor backup` lists them
separately from user-initiated backups so the user knows which
were tool-created and prunable.

If the user cares about an older pre-migration snapshot — perhaps
to attempt a downgrade two upgrades back — they `mv` it out of the
auto-managed directory.
