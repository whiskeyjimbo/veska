---
id: ADR-S0011
title: Two-pool single-writer model via database/sql
status: accepted
date: 2026-05-09
deciders: [whiskeyjimbo]
---

# ADR-S0011 — Two-pool single-writer model via `database/sql`

## Context

SQLite in WAL mode permits many concurrent readers but exactly one
writer at a time at the OS lock level. `sqlite-vec`'s `vec0` virtual
table writes go through that same lock. Whatever model we pick,
serialization is a physical requirement — the question is which
layer owns it and how backpressure surfaces.

The first draft of SOLO-11 §10 specified an explicit
`promotion_coordinator` goroutine with a `WriteOp` channel: every write
was an envelope sent to a single drainer that held the only write
transaction. Two problems with that shape:

1. **It clashes with the repository ports.** SOLO-04 §11 says every
   aggregate root has `Save(ctx, *Entity) error`. A `Save` whose
   only job is to package a `WriteOp` and wait on a reply channel
   makes the port shape a thin wrapper over a homemade queue, and
   reads as ceremony.
2. **Embed batches head-of-line promotions.** A `vec0` upsert of a
   50k-symbol refactor's embeddings can hold the writer for
   hundreds of milliseconds to seconds. Routed through one
   coordinator FIFO, it stalls every promotion and MCP write behind it.
   Adding priority lanes to fix that re-grows the multi-tenant
   queueing surface this redesign deletes elsewhere.

A second option — pure SQLite-native, every goroutine opens its own
`BEGIN IMMEDIATE` and lets the OS lock serialize — is idiomatic but
makes backpressure invisible: there is no queue depth to read, no
metric, no place to surface "the writer is busy" cleanly.

The third option, and the one we adopt, is to let `database/sql`'s
connection pool be the queue. A `*sql.DB` with `SetMaxOpenConns(1)`
serializes **transaction acquisition** at the pool: every
`BEGIN IMMEDIATE`-to-`COMMIT` window holds the connection
exclusively, so transactions on the same pool run one at a time.
This is *transaction-grain* serialization, not statement-grain —
two consecutive `BEGIN; INSERT; COMMIT` cycles take the connection
in turn, with no nesting and no statement interleaving. We keep
the repository port shape; we get the metric surface for free; we
solve the head-of-line problem by opening a second writer pool for
the embed worker, which SQLite then serializes against the first
under its own OS-level lock.

**`*sql.DB` handles do not cross into application code.** The
infrastructure adapter (the sqlite repository) holds the pool;
application code holds the repository port (SOLO-07 §5). The
prior wording "injected into the infrastructure adapters" is
preserved here verbatim from the original draft, but is meant
strictly: the bootstrap composition root opens the pools and
hands them to the sqlite adapters; nothing else sees them.
`PostPromotionQueueRepository` is the seam through which the application
layer's post-promotion queue drains read and write rows without holding a
handle.

## Decision

**Three `*sql.DB` handles to one `veska.db` file.** All three are
opened by `infrastructure/sqlite/pools.go` (`OpenPools`) at
bootstrap time. The bootstrap call site receives a `*Pools`
struct of opaque fields and passes it into the sqlite adapter
constructors; **the handles never enter `application/` or
`core/`**.

| Handle | `MaxOpenConns` | Used by |
|---|---|---|
| `readDB` | unlimited (pool default) | All read paths: MCP query handlers, doctor, post-promotion queue poll-reads, resolver chain |
| `writeDB.hot` | **1** | Promotion transaction; MCP write tools; post-promotion queue state transitions for `auto_link` / `revalidate` / `review`; migrations (before serving begins) |
| `writeDB.embed` | **1** | Embed worker only: `node_embeddings` inserts, `node_embedding_refs` updates, `vec_nodes` upserts |

Properties:

1. **One writer per pool, two pools total.** Each `writeDB` with
   `MaxOpenConns=1` queues writes at the connection-pool layer.
   When both pools have an in-flight transaction, SQLite's OS-level
   writer lock serializes them — `BEGIN IMMEDIATE` on the second
   pool's connection blocks until the first pool commits. There is
   no goroutine-level coordination; the standard library and SQLite
   between them do all the queueing.

2. **Repository ports keep their shape.** `Save(ctx, *Entity)
   error` opens a transaction on `writeDB.hot`, writes, commits.
   No envelope, no reply channel, no `WriteOp` type.

3. **`BEGIN IMMEDIATE` is mandatory.** Every write transaction
   uses `BEGIN IMMEDIATE` so the writer lock is acquired up front;
   we never risk `SQLITE_BUSY` on a deferred-tx upgrade.

4. **`busy_timeout` per pool.** Set via `PRAGMA busy_timeout` on
   each connection's open hook (DEFAULT, CONFIG-SURFACE.md):
   - `writeDB.hot`: **5000 ms**. Hot path; we'd rather fail fast
     than queue invisibly under sustained load.
   - `writeDB.embed`: **30000 ms**. Embed batches expect to wait
     for the hot path; this is the embed worker's normal mode.
   - `readDB`: **5000 ms**. Reads in WAL mode rarely block, but
     we set a value to bound any pathological case.

5. **Backpressure is `database/sql`'s wait queue.** Callers honor
   `ctx`; if the pool can't allocate a connection within the
   deadline, the caller sees the standard `context.DeadlineExceeded`,
   which the application layer translates into `ErrBusy`
   (SOLO-09 §4.6) carrying the state from `sql.DBStats()`
   (`InUse`, `WaitCount`, `WaitDuration`) so the editor sees a
   meaningful "queue is busy" signal.

6. **Metrics for free.** `db.Stats()` on each handle gives us
   `WaitCount`, `WaitDuration`, `MaxIdleClosed`, etc. Exposed under
   the standard slog attribute set (SOLO-13 §1.3). No homemade
   queue-depth gauge to maintain.

7. **Read paths never wait on writers.** WAL mode plus a separate
   `readDB` with normal pooling means MCP query handlers see a
   consistent snapshot for the duration of their transaction
   without contending for the writer lock.

8. **The promotion transaction is just a write.** Post-commit hook
   calls `veska promote` over the Unix socket; the daemon side opens
   a transaction on `writeDB.hot`, runs the promotion SQL (SOLO-08 §5),
   commits. There is no "promotion coordinator" type. The promotion pipeline
   in `application/` is plain Go calling repository methods.

9. **post-promotion queue drains.** Three of the four `work_kind`s
   (`auto_link`, `revalidate`, `review`) write findings/state via
   the hot pool — their writes are short SQL and rare relative to
   promotions. Only `embed` writes are routed to `writeDB.embed`. The
   post-promotion queue `state` transition itself (`pending → in_progress →
   done`) is also a short write; each drainer takes the hot pool
   for the transition and (for embeds) the embed pool for the
   payload work. This is two small writes per post-promotion queue row, both on
   pools designed for them.

10. **Migrations run before serving.** The migration runner in
    `bootstrap/daemon.go` uses `writeDB.hot` after it is opened
    and before any other goroutine starts. Once migrations finish,
    serving goroutines spin up.

11. **One-bit promotion barrier.** The promotion path is on the same
    `writeDB.hot` pool as MCP writes, so a burst of agent-driven
    flips (e.g., ten `close_finding` calls) arriving just before
    a `git commit` would queue ahead of the promotion at the pool and
    erode the SOLO-13 §3.1 typical-commit budget. To avoid this
    without rebuilding a priority lane, the daemon raises a
    single-bit barrier when the `Promote` RPC arrives. New MCP
    write tool entrants wait on the barrier; in-flight
    transactions complete normally; the promotion then takes the
    connection in normal FIFO order and drops the barrier on
    commit or rollback. The barrier is not refcounted (promotions do
    not stack — see SOLO-11 §2.3 short-circuit) and carries no
    work-class metadata. SOLO-11 §10.2 specifies the runtime
    contract.

## Consequences

Positive:

- Repository ports compile to real adapters that look like normal
  Go SQL code. New developers do not have to learn a homemade
  queueing API to write a write tool.
- The "embed batches do not stall promotions" property is a structural
  consequence of having two pools, not a discipline we have to
  maintain in scheduler code.
- Backpressure metrics are standard library output, not custom
  instrumentation.
- Restart recovery is unchanged: the promotion transaction's atomicity
  guarantees still hold; post-promotion queue drainers re-claim `in_progress`
  rows on startup the same way ADR-S0004 specifies.
- `sqlite-vec` extension semantics — that `vec0` writes take the
  same lock as table writes — work in our favor here. The two
  pools serialize via that lock without us writing a line of
  scheduling code.

Negative:

- Three `*sql.DB` handles means three sets of pool stats, three
  PRAGMA initialisations, three places to remember to set
  `busy_timeout`. Mitigated by a single `openSQLite(role)` helper
  in `infrastructure/sqlite/`.
- Less obvious to a reader than "one writer goroutine, one
  channel." The cost lands in documentation: SOLO-11 §10
  describes three pools instead of one channel.
- We give up the ability to enforce "only this code path may
  write." Any package that imports the hot writeDB can write. We
  rely on `tools/lint/layercheck` and code review, not types.
- Two-pool design means the embed worker can hold the OS writer
  lock long enough to make the hot pool's `busy_timeout` matter
  in practice. Under a 50k-symbol refactor's embed batch, hot-path
  writes will queue at the SQLite layer for the duration of an
  embed transaction. The embed worker chunks `vec0` upserts into
  small batches (≤256 rows) so each individual transaction is
  bounded; the hot pool's 5s `busy_timeout` then covers
  worst-case contention.

  The "interleave at COMMIT boundaries" framing assumes idle time
  *between* embed chunks. A sustained refactor storm (≈196 chunks
  for 50k symbols) without idle gaps means a hot writer arriving
  mid-storm faces a chain of OS-lock acquisitions, not one. If
  the per-chunk commit time × chunks-during-busy-window exceeds
  5s, the hot writer hits `busy_timeout` and the MCP write
  surfaces `ErrBusy`. Bench gate: M1's writer-contention
  measurement (SOLO-13 §3.4) validates the chunk size against
  this scenario, *not* against a single-chunk worst case.
  **Fallback path if M1 shows the chain blows the budget:** the
  embed worker yields the OS lock (sleeps a configurable
  inter-chunk pause) when `seal_pending` is set or when the hot
  pool's `WaitCount` is non-zero — the embed throughput drops,
  the promotion latency holds. Chunk-size shrinking is the secondary
  lever; the yield is the primary one because it preserves
  throughput while idle.
- Promotion and MCP writes share `writeDB.hot`. Without a barrier, a
  burst of MCP writes arriving just before a `git commit` would
  queue ahead of the promotion and erode the §3.1 typical-commit
  budget (each finding-state flip is low milliseconds, but ten
  of them is a meaningful fraction of 100ms). Mitigated by
  property #11 (promotion barrier) — a single boolean that gates
  *new* MCP write entrants once the `Promote` RPC arrives. This is
  not a priority lane: there is no per-kind queue and no
  fairness scheduler, just one bit.

## Alternatives Considered

- **Explicit `promotion_coordinator` goroutine + `WriteOp` channel.**
  Rejected: rebuilds priority lanes when embed batches arrive,
  re-grows the multi-tenant queueing surface, and clashes with
  the repository port shape. The promotion-vs-MCP-write contention
  this would have addressed is instead handled by the one-bit
  promotion barrier (property #11) — a single boolean, not a
  scheduler.
- **Pure SQLite-native, every goroutine opens its own
  `BEGIN IMMEDIATE`.** Rejected for one reason: backpressure is
  invisible. No queue depth, no `WaitCount`, no clean way for
  the application layer to translate "writer is busy" into an
  MCP error. We would end up writing a wrapper, which is what
  this ADR is anyway.
- **One writeDB shared by everything (no embed split).** Rejected:
  embed batches stall promotions. The point of having two pools is to
  separate the latency-sensitive path from the throughput-sensitive
  one without introducing a scheduler.
- **Four pools (one per post-promotion queue `work_kind`).** Over-engineering.
  `auto_link`, `revalidate`, `review` writes are short and rare;
  they share the hot pool with no observable contention.

## References

- SOLO-08 §3.4 (`post_promotion_queue`), §5 (promotion transaction), §6 (failure modes)
- SOLO-11 §10 (write serialization narrative)
- SOLO-04 §11 (repository port shape)
- SOLO-07 §3 (package layout — the `bootstrap/daemon.go` wires the three handles)
- ADR-S0001 (SQLite + sqlite-vec substrate)
- ADR-S0004 (post-promotion queue `work_kind` and drain semantics)
