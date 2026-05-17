---
id: ADR-S0007
title: Embedder swap is one CLI subcommand against a live daemon
status: accepted
date: 2026-05-09
deciders: [whiskeyjimbo]
verified: true
verified_date: "2026-05-16"
---

# ADR-S0007 — Embedder swap is one CLI subcommand against a live daemon

## Context

Embedding models change. Users want to switch from
`nomic-embed-text:v1.5` to a newer model, or from CPU Ollama to a
hosted provider. The prior V2 design specified a five-phase
migration ceremony — `Planned → DualWrite → Shadowing → Cutover →
Complete` — with a per-row migration cursor, dual-index reads, and
a documented rollback. That ceremony was sized for a multi-tenant
server with downtime budgets and SLAs.

A single-user laptop has no SLA. The user's editor will be slow for
a while during the rebuild. They know it. But "stop the daemon,
edit the config, hand-craft SQL, restart" is not a UX — it is an
OS-level surgery the user is not in a position to script
correctly. In particular, the database's `vec_nodes` virtual
table geometry, `database_meta.embedder_*`, and the
`config.toml` `[embedder]` block must change atomically;
mismatch leaves the daemon refusing to start (SOLO-08 §3.3,
`ErrEmbedderMismatch`). A single subcommand against the live
daemon does the right thing.

## Decision

The user runs:

```
veska embedder swap <model>                    # pulls model dim from a probe
veska embedder swap --provider=ollama --model=mxbai-embed-large
veska embedder current                          # prints active provider/model/dim
```

The CLI sends a control RPC to the daemon. The daemon executes
the sequence specified in **SOLO-03 §3.2 ("`veska embedder
swap <model>`")**:

1. Probe the new model via Ollama; read its embedding dim.
   Refuse if the model is not pulled.
2. Stop accepting MCP writes (reads continue against promoted
   state). Tag responses with
   `degraded_reasons:["embedder_swapping"]`.
3. Take an auto-snapshot using the migration runner's snapshot
   path (SOLO-08 §10) to `~/.veska-backups/pre-swap-...`.
4. In one transaction on `writeDB.hot`:

   ```sql
   DROP TABLE vec_nodes;
   DELETE FROM node_embeddings;
   UPDATE node_embedding_refs SET state = 'pending', content_hash = NULL;
   UPDATE database_meta SET value = ?, set_at = ? WHERE key = 'embedder_provider';
   UPDATE database_meta SET value = ?, set_at = ? WHERE key = 'embedder_model';
   UPDATE database_meta SET value = ?, set_at = ? WHERE key = 'embedder_dim';
   CREATE VIRTUAL TABLE vec_nodes USING vec0(content_hash TEXT PRIMARY KEY, embedding FLOAT[<new_dim>]);
   ```

5. Edit `~/.veska/config.toml` `[embedder]` in place to match
   `database_meta`.
6. Resume MCP writes.

The standard embed worker drains the `pending` rows at the new
model's natural throughput. `veska doctor post-promotion-queue` shows
progress; semantic search responses surface
`degraded_reasons:["embedding_pending"]` until the queue drains.

The `model` column on `node_embeddings` is preserved for two
reasons: (a) sanity assertion that no row predates the swap;
(b) a future ADR may use it to support hybrid configurations.
At V2.0 we do not branch on it; the swap drops every row.

## Consequences

Positive:

- One CLI subcommand against a live daemon. No "stop the
  daemon, edit raw SQL, restart" surgery.
- Atomic schema/meta change; pre-swap auto-snapshot covers
  rollback.
- The daemon never has to handle two models being live at once,
  which removes an entire class of correctness bugs.
- Boot consistency check (SOLO-08 §3.3) makes a swap performed
  with the daemon down (e.g. user edited config manually) fail
  loudly on next start with `ErrEmbedderMismatch` instead of
  silently producing wrong embeddings.

Negative:

- During rebuild, semantic search is degraded. For a 1M-node repo
  on CPU Ollama, that window is hours. We surface the degradation
  via `degraded_reasons` in MCP responses; the user can read
  progress in `veska doctor`.
- A user who switches models often will pay the rebuild cost often.
  Not our problem; if they need fast switching, they can keep the
  old database and toggle config files.

## Alternatives Considered

- **Five-phase migration ceremony (prior ADR-0015).** Justified
  only with downtime budgets we do not have. Retracted.
- **Dual-write during a transition window (no shadowing).** Doubles
  embedder throughput requirements during the window for no
  user-visible benefit; lexical search already covers the gap.
- **Lazy re-embed on read.** Random `find_similar_symbols` calls
  trigger embedder work; latency becomes unpredictable. The
  background rebuild gives steady, predictable throughput.

## References

- SOLO-03 §3.2 (`veska embedder swap <model>` operational sequence)
- SOLO-08 §3.3 (`database_meta` boot consistency check; `vec_nodes` geometry)
- SOLO-08 §7 (what this design does NOT include — embedder migration ceremony)
- ADR-S0004 (the post-promotion queue the rebuild drains through)
- Retracts ADR-0015 (see RETRACTED.md)
