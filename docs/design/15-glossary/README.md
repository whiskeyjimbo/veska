---
id: SOLO-15
title: "Glossary - Veska Solo"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
verified: true
verified_date: "2026-05-16"
---

# SOLO-15 - Glossary

The authoritative ubiquitous language. Italicized on first use per
doc. If a term you need is not here, either use plain English or
add it via PR.

## Core entities

| Term | Meaning |
|---|---|
| **Node** | A symbol in the codebase: function, method, type, file, or package. Identified by stable hash of `(repo, language, symbol_path)`. |
| **Edge** | A typed relation between two nodes: `CALLS`, `IMPORTS`, `CONTAINS`, `TESTS`, `DEPENDS_ON`, plus `SIMILAR_TO` - the auto-link kind emitted by the auto-link pipeline (`Confidence=Unresolved`, paired with a `source_layer='semantic'` Finding). Six kinds. **Same-Graph, absolute** (SOLO-04 §5.2 invariant 1): every stored `Edge` has both endpoints in the source repo's `nodes` - `dst_node_id` is `NOT NULL` and FK-checked. Cross-repo edges are never stored in `edges`; they are computed at query time (synthetic). |
| **CrossRepoStub** | A value object owned by a source `Node` (SOLO-04 §5.2.1), stored in the sibling `cross_repo_edge_stubs` table (SOLO-08 §3.1). Recorded at promotion time when a parser produces a cross-repo reference whose target repo is not yet indexed; carries `(kind, module_path, symbol_path, language)`. The resolver projects stubs into MCP responses as synthetic unresolved edges at read time. **Not an `Edge`** - keeps the `Edge` same-Graph invariant absolute. |
| **Graph** | The in-memory read projection of `Node` + `Edge` for a given `(repo, branch)` scope (SOLO-04 §5.3). **Not an aggregate root** - `Graph` has no write methods. Writes are row-shaped through `GraphRepository.SaveNode` / `SaveEdge` / `DeleteFile` (SOLO-04 §11.2). The graph-shaped object exists where it earns its keep - the resolver chain (SOLO-11 §9), blast-radius, and call-chain analysis - and is materialised by `GraphRepository.LoadGraph`. The "same-Graph" invariant for edges (SOLO-04 §5.2 inv 1) is enforced at the row level by `(repo_id, branch)` scope plus the SOLO-08 §3.1 FK. |
| **Cross-repo edge (synthetic)** | A conceptual edge between nodes in different repos (e.g. service A `CALLS` SDK B). Computed at query time by the resolver chain (SOLO-11 §9), under the per-query `as_of` snapshot per target repo (SOLO-04 §5.4 invariant 2). Returned in MCP responses tagged `cross_repo: true` with the snapshot tuples in `as_of`. |
| **`as_of` envelope** | The tuple `[{repo_id, branch, promoted_sha}, ...]` returned with any traversal that touches more than one repo. Names the per-target promoted state the resolver read against, so a cross-repo result is reproducible. SOLO-04 §5.4. |
| **Blast radius** | The transitive closure of nodes reachable from a starting node by following `CALLS` and `DEPENDS_ON` edges, bounded by depth and repo scope. Returned by `get_blast_radius` and `get_dirty_blast_radius`. Interface-mediated calls are not modelled in V2.0 (see SOLO-04 §5.2). |
| **Task** | A unit of work, often (but not always) pinned to a tracker issue. Carries `active: bool` so MCP can scope to "what I'm working on right now". |
| **Finding** | A surfaced concern: a vuln, a dead-code warning, a contract drift, a structural rule violation. Carries `source_layer ∈ {structural, semantic, security, quality}`. |
| **Suppression** | A user/agent decision to silence a finding. Scoped to symbol, file, repo, or finding-id. |
| **Actor** | The pair `(actor_id, actor_kind)` stamped on every state-changing write. Not the Erlang/Akka Actor pattern; this is purely an attribution stamp (SOLO-04 §10a). |

## Storage / runtime

### Promote - the term family

The promote/promotion family is load-bearing across the doc
tree. The terms were renamed from *Seal* per ADR-S0012 because
the prior single word served as verb, noun, and adjective root -
unworkable for new readers. The new family separates the roles:

| Form | Part of speech | Meaning |
|---|---|---|
| **promote** (verb) | verb | The act of moving staging rows to durable SQLite atomically. "The post-commit hook promotes staging." |
| **Promotion** (noun) | noun, count | One transactional execution of *promote*. Each `git commit` produces one Promotion. |
| **`promotion_id`** | identifier | ULID assigned to one Promotion instance. Carried in the `post_promotion_queue` rows that Promotion enqueues, in audit lines, and in slog attributes. Stable forever; never reused. |
| **promoted** (adjective) | adjective | Property of a row, file, or graph: "this came from durable on-disk SQLite, not staging." Antonym: *staged*. |
| **promoted state** | noun phrase | The full on-disk SQLite graph at any instant. What MCP reads see when staging is absent. |
| **`promoted_sha`** | identifier | The Git commit SHA that a Promotion corresponds to. Stored on `repos.last_promoted_sha` and on every Promotion row. Distinct from `promotion_id` - `promotion_id` is *our* identity, `promoted_sha` is *Git's*. |
| **Promotion pipeline** | proper noun | The synchronous structural checks (dead-code, secrets, vuln, contract-drift) that run inside the Promotion transaction or immediately after, before hook return. SOLO-11 §2. |
| **Post-promotion queue** | proper noun | The durable work queue that Promotion writes to (see entry below). Drained asynchronously after Promotion commits. |

The mnemonic: **promote (verb) → Promotion (noun) → promoted
(adjective).** The verb takes staging to disk; the noun is one
such event with a `promotion_id`; the adjective describes
anything the verb has already touched (`promoted_sha`,
*promoted state*, *the promoted graph*).

### Other storage / runtime terms

| Term | Meaning |
|---|---|
| **Staging** | The in-memory overlay that holds unpromoted edits. Lost on restart by design. |
| **Post-promotion queue** | A SQLite table (`post_promotion_queue`) that holds work to be done after promotion: embed, auto-link, revalidate. Drained by one goroutine per `work_kind`. Historically called "outbox" - that name was retired because the transactional-outbox vocabulary implies microservices eventual consistency, while this table is read by a goroutine in the same process. The durable-queue shape is the same; the architectural connotation isn't. |
| **Embedding** | A 768-dim float vector keyed by `content_hash`. Held in the in-memory vector store (memvec) by default. |
| **`node_fts`** | The FTS5 virtual table (SOLO-08 §3.3) that backs the lexical fallback for `eng_search_semantic` when the embedder is unreachable. Always populated; written inside the promotion transaction. |
| **Deferred (post-promotion queue state)** | A `post_promotion_queue` row state used for `embed` rows enqueued at queue depth ≥ `post_promotion_queue.high_water` (SOLO-08 §3.4). Promotion proceeds; the row transitions to `pending` when depth drops below low-water. The escape path that prevents embed-pause × post-promotion-queue-full deadlock. |
| **Source layer** | A tag on `Finding` and (some) `Edge` rows: `structural | semantic | security | quality`. Closed enum. |

## Process / lifecycle

| Term | Meaning |
|---|---|
| **Save** | Any interactive write (editor save, agent edit, `veska index`). Updates staging only. |
| **Commit** | The Git operation. Triggers the post-commit hook, which triggers promotion. |
| **Hook return** | The post-commit hook returns to Git. Budget rows in SOLO-13 §3.1 (split typical vs. refactor commit). |
| **Drain** | The async work after promotion: embedding, auto-link, revalidation. |

## Pipelines

| Term | Meaning |
|---|---|
| **Save pipeline** | Tree-sitter reparse on fsnotify, update staging. In-memory only. |
| **Promotion pipeline** | See the "promotion - term family" block above. |
| **Review pipeline** | Optional LLM-driven review (security, contract drift) that runs as a goroutine after promotion. Off by default. |


## Plugin-swappable ports

| Term | Meaning |
|---|---|
| **Port** | A Go interface in `core/ports/`. SOLO-07 §4 catalogues all 19: 4 repository + 2 storage adjuncts + 12 substrate (Logger non-swappable) + 1 driving port (`RPCHandler`, §4.3a). SOLO-05 covers the 11 substrate ports that are plugin-swappable. Hex direction matters: *driven* ports are called by the application and implemented by adapters; *driving* ports are called by adapters and implemented by the application. Both live in `core/ports/` so the import rule "infrastructure never imports application" stays absolute. `StagingArea` and `PostPromotionQueueDrainer` are not ports - they are plain Go interfaces in `application/` for intra-layer testability (no hex direction crossed). |
| **Driving port (inbound port)** | A port called by an adapter and implemented by the application. `RPCHandler` is the only one in V2.0: the UDS transport adapter holds the port; the application's top-level router implements it (composing MCP and Control sub-routers internally). A second MCP transport (gRPC, named pipe) would be a second *driving adapter* - not a second port. SOLO-07 §4.3a. |
| **Token estimator** | The port (SOLO-05 §2.11) that estimates response token counts. Default impl is `chars/4`; documented as approximate. The `ModelHint()` is recorded in audit lines for any truncated response. |
| **Tracker** | The port for issue-tracker integration. Default: `none` (off). The only shipped non-`none` impl is `bd-cli`, opt-in via `veska init` with a probe for `bd` on `$PATH`. We refer to the integration as "the tracker" or "the bd-cli tracker"; the brand "Beads" never appears in normative prose. |
| **Vuln source** | The port for vulnerability-feed integration. Default impl: none (off). |
| **Embedder** | The port that turns text into a vector. Default impl: Ollama with `nomic-embed-text`. One impl. |
| **LLM generator** | The port that runs LLM completions. Default impl: Ollama. One impl; hosted providers come behind a future ADR. |
| **Notifier** | The port for findings notifications. Default impl: stderr (the editor sees them via MCP). |

## Identity & gating

| Term | Meaning |
|---|---|
| **`actor_id`** | A column on every aggregate write. One of `human:<user>`, `agent:<name>`, or `service:veska`. Free-form label; not trusted. |
| **`actor_kind`** | An enum column on the same row: `'human' \| 'agent' \| 'system'`. Daemon-set from the accepting listener; a typical MCP client cannot supply or override it. The single human-action-gate input. |
| **Human-action gate** | `close.finding[severity >= high] requires actor_kind = 'human'`. The only gate. A UX and audit guardrail, not a security boundary against same-user code; the OS user is the privilege boundary. SOLO-10 §3.1. |

## Operational

| Term | Meaning |
|---|---|
| **`veska doctor`** | The CLI subcommand that reports daemon health, config, and egress. The primary operator surface. |
| **`audit.jsonl`** | The append-only audit log under `~/.veska/`. Plain JSONL. Forward elsewhere if you need more. |
| **Degraded** | A query that returned partial results because a dependency was unavailable. Carries `degraded_reasons: [...]` in the MCP response. |

## What is NOT here (and why)

The terms below are not part of the solo vocabulary. They name
concepts the solo product does not have.

- **Composite identity / acting-as / on-behalf-of** - one user.
- **Workspace** as a first-class entity - use "repo" and "session".
- **Tenant** - single-user product.
- **Canonical tier / `[L]` / `[W]` / `[C]`** - single tier, no modes.
