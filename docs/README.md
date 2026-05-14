# Veska — Design Set

> **Naming note.** "Engram" is the product name. "Solo" is the
> single-user, single-daemon shape this design set covers. Calling
> this a "Solo edition" is descriptive, not a skew implying a
> "Pro" tier — there is no other tier today. Server-tier work is
> out of scope for this design set (ADR-S0009); a future ADR could
> bring it back, at which point the trademark deadline in
> SOLO-01 §6 applies.

One daemon, one developer, one machine. SQLite + sqlite-vec for
storage; tree-sitter for parsing; Ollama for embeddings; MCP over
Unix sockets for the editor surface. Everything Engram knows lives
in `~/.veska/`.

## Read order

1. [`PRODUCT.md`](PRODUCT.md) — what Engram is, in plain English.
2. [`design/00-README.md`](design/00-README.md) — design-set conventions.
3. [`design/01-charter/README.md`](design/01-charter/README.md) — binding pillars.
4. [`design/03-the-daemon/README.md`](design/03-the-daemon/README.md) — runtime topology.
5. [`design/08-data-and-storage/README.md`](design/08-data-and-storage/README.md) — the SQLite substrate.
6. [`design/04-domain-model/`](design/04-domain-model/) — entities and ports.
7. [`design/11-pipelines/`](design/11-pipelines/) — what runs on save vs. promotion.
8. [`design/09-mcp-surface/`](design/09-mcp-surface/) — the editor-facing API.
9. [`milestones/`](milestones/) — work-breakdown into PR-sized epics.

**Sidebar — read alongside, not after.** [`design/15-glossary/README.md`](design/15-glossary/README.md)
is the canonical home for the project's vocabulary. Several
terms are load-bearing and overloaded — *promotion* (verb / noun /
identifier / adjective), *staging*, *promoted_sha* vs. *promotion_id*,
*Actor*, *source_layer* — and lose their precision the moment
they get paraphrased. If a term in the docs above lands raw,
the glossary's term-family blocks are the disambiguator. Keep
it open in a second pane while reading the rest.

## Design principles (in priority order)

1. **One user, one daemon, one machine.** No tier above. No tier below. No "future-proofed" port shapes for tiers we are not building.
2. **YAGNI is the default.** The first impl of every port is the only impl. A second impl is a future ADR, not a present abstraction.
3. **Measure before budget.** Embedding throughput, Ollama warmup, SQLite write contention, and tree-sitter reparse cost get measured before any number commits to the design. Every performance number is labelled (`BUDGET (unmeasured)` / `BUDGET (measured M<N>)` / `INVARIANT` / `DEFAULT`).
4. **Plain names.** Patterns are named after what they are: goroutines, channels, tables, Go interfaces. Where inherited vocabulary would mislead (e.g., the "transactional outbox" pattern carries microservices/saga connotations that don't apply to a single-process goroutine reading a SQLite table — so we call ours `post_promotion_queue`; the "Actor" stamp is not the Erlang Actor model and the doc says so), the design renames or disambiguates rather than ducking the collision.
5. **The CLI is the operator surface.** `veska doctor` is the only thing the user actually sees. Spec it before its observability backend.
6. **Small surface.** Ship the surface area you have to maintain. Anything bigger is a future ADR with a measured trigger.
7. **Deferred work stays out of normative text.** Anything not shipped lives under `deferred/` and is not referenced from normative sections.

## Status

Draft. Nothing here is approved. The design starts narrow on
purpose; it grows only on evidence.
