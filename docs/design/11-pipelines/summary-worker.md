---
id: FEATURE-SUMMARY-001
title: "Summary Worker — LLM-generated node short_summary"
status: draft
version: 0.1.0
last_reviewed: 2026-05-22
related: [SOLO-04, SOLO-05, SOLO-08, SOLO-09, SOLO-10, SOLO-11]
verified: false
---

# FEATURE-SUMMARY-001 — Summary Worker

A background pipeline that attaches a short natural-language summary to
every promoted `Node` and keeps it fresh as source changes. The summary
is what MCP tools return by default (the SOLO-09 §4.1 summary
projection), so its quality directly drives how much an agent
understands from a 50–100 node response without paying for full bodies.

This is a carry-forward from engram-v2 (DV2-summary subsystem). The
field and the default-projection slot already exist in solov2; today the
summary is **heuristic-only**. This doc specs the LLM upgrade: a
`SummaryWorker` that runs on the `[llm_generator]` slot as an optional
post-promotion queue lane, exactly mirroring the review pipeline.

## 1. Motivation

- **Token economy.** SOLO-09 §4.1 already returns `{node_id, name,
  signature, summary}` by default. A good summary is the difference
  between an agent grasping a symbol from one line and having to fetch
  `include_body: true` for every hit.
- **The field is specced but unfilled.** `domain.Node` carries
  `Signature`, `RawContent`, `ContentHash`, … but **no summary field**
  yet (`internal/core/domain/node.go:47`), and SOLO-09 §4.1 currently
  describes `summary` as `<heuristic, ≤ 280 chars>`. This feature adds
  the field and an LLM producer for it.
- **Precedent exists.** The review pipeline (SOLO-11 §3) is already an
  optional, off-by-default, LLM-backed post-promotion lane. Summary
  reuses that machinery wholesale — no new architectural pattern.

## 2. Domain model

Add one optional field to `Node` (DV2-04.02 §1.3 carry-over):

```go
type Node struct {
    // … existing fields …
    ShortSummary *string // heuristic or LLM-generated, ≤ 280 chars
}
```

Plus a functional option `WithShortSummary(string) NodeOption`, matching
the existing `WithSignature` / `WithRawContent` style.

**Invariants**

1. `len(*ShortSummary) ≤ 280` runes when present (the SOLO-09 budget).
2. Provenance is mandatory: a summary written by the LLM lane is
   attributed `actor_id = "agent:<llm-generator-name>"`,
   `actor_kind = "system"` (SOLO-10 §1.2 review-pipeline exception —
   a daemon goroutine writes the row, but an LLM produced the content).
3. `ShortSummary` MAY be nil before the summary lane runs. Unlike
   engram-v2's "never null after seal" rule, solov2 promotion stays
   synchronous and structural (SOLO-11 §2); summary is async
   post-promotion work, so a freshly-promoted node legitimately has no
   summary until its lane drains. Readers fall back to the heuristic.

## 3. Storage

`nodes.short_summary TEXT NULL` (SOLO-08 graph tables). Written by the
summary `WorkHandler` inside the post-promotion transaction lane, not by
the parser. The default node projection reads this column directly; when
NULL, the read path computes the heuristic summary on the fly so the
contract field is always populated in responses.

## 4. The summary queue lane

Summary is a new **optional** `work_kind`, gated like review.

### 4.1 WorkKind

`internal/core/ports/queue.go` — add:

```go
WorkKindSummary WorkKind = "summary"
```

`work_kind` TEXT column already accepts new values structurally
(SOLO-08 §; "new kinds are added here first and the infrastructure
poller picks them up"). The always-on set in
`application/promotion_store.go` stays `{embed, auto_link, revalidate}`;
summary joins `review` as an opt-in lane appended by
`PromotionWorkKinds(...)` when enabled.

### 4.2 Handler

`application/summary/handler.go` implements `ports.WorkHandler` for
`WorkKindSummary`, one row per changed file (or per node — see §9 OQ-1):

```
Handle(ctx, row WorkRow):
  1. Load the promoted nodes for row.{RepoID,Branch,GitSHA}.
  2. For each node whose content_hash changed since its last summary:
       a. Build a prompt from {signature, raw_content (capped), kind, path}.
       b. summary := LLMGenerator.Generate(ctx, prompt)   // [llm_generator] slot
       c. Truncate to 280 chars on a rune boundary.
       d. Persist nodes.short_summary with system+agent provenance.
  3. Return nil on success; a non-nil error re-queues (attempts < 3).
```

Handlers must be safe for concurrent use; the poller runs one goroutine
per `work_kind` (queue.go:42 contract).

### 4.3 Trigger & freshness

Enqueued post-promotion alongside the other lanes. Re-generation is
keyed on `content_hash`: a node whose hash is unchanged since its last
summary is skipped, so a no-op promotion does not burn LLM calls. This
mirrors the embedding lane's content-hash cache key.

## 5. Compute slot

Runs on the `[llm_generator]` slot (SOLO-05 §; `provider = "ollama"` by
default), the **single-cardinality** slot already defined for the review
pipeline. Single-slot is deliberate: if two generators wrote
`short_summary`, a reader couldn't tell which model authored it without
a provenance join. A future "compare summaries from N models" UX is a
slot-cardinality ADR, not a per-impl flag.

The `LLMGenerator` port (`internal/core/ports/llmgenerator.go:77`)
already exposes generation; the worker consumes it, it is not a new
port.

## 6. MCP surface

**No new tool.** Summary is already the default node projection
(SOLO-09 §4.1). This feature only improves the *content* of the existing
`summary` field. Tools returning nodes (`eng_get_node`,
`eng_find_symbol`, `eng_search_semantic`, …) transparently surface the
better summary. (`eng_get_review_summary` in SOLO-09 §future is an
unrelated work-state tool — not this.)

## 7. Degradation

`[llm_generator]` defaults to Ollama, a single point of failure shared
with embeddings and review. When Ollama is unhealthy the summary lane
**skips** (leaves `short_summary` NULL → heuristic fallback fills the
response) and the row is re-queued, rather than failing the promotion.
Responses that would have carried an LLM summary but fell back are not
specially flagged at the node level; the daemon's existing
`external_source_unhealthy:ollama` degraded indicator (SOLO-13) tells
the user one thing is wrong. Promotion itself never blocks on summary.

## 8. Gating & rollout

Off by default, like review. A workspace-config flag
(`pipelines.summary.enabled`) appends `WorkKindSummary` to the
post-promotion lanes. When disabled, every node response uses the
heuristic summary — i.e. today's behaviour, unchanged. This makes the
feature a pure additive opt-in with a clean fallback.

## 9. Open questions

- **OQ-1: row granularity.** One `summary` row per file (batch the
  file's nodes into one or few LLM calls) vs one per node (simpler, more
  calls). Review enqueues per-file; summary likely wants per-file
  batching to amortise LLM latency. Decide before implementation.
- **OQ-2: heuristic definition.** SOLO-09 §4.1 already promises a
  heuristic summary; its exact derivation (first doc line? signature +
  kind?) should be pinned so the fallback is deterministic and the LLM
  upgrade is measurable against it.
- **OQ-3: prompt + eval.** A summary-quality eval (analogous to the
  recall gate) to confirm the LLM summary beats the heuristic enough to
  justify the per-node LLM cost. Gate before defaulting on.
- **OQ-4: cost ceiling.** Large promotions could enqueue thousands of
  summary calls. Need a per-promotion budget / backpressure story shared
  with the embedding lane.

## 10. Acceptance criteria

1. `domain.Node.ShortSummary *string` + `WithShortSummary` option, with
   the ≤280-rune invariant enforced at construction.
2. `nodes.short_summary` column + read path; default projection returns
   the stored summary, falling back to the heuristic when NULL.
3. `WorkKindSummary` lane + `summary.Handler` (`ports.WorkHandler`),
   wired into the daemon poller, gated by `pipelines.summary.enabled`
   (default off).
4. Content-hash-keyed skip so unchanged nodes are not re-summarised.
5. Provenance: LLM summaries written with `actor_kind="system"`,
   `actor_id="agent:<llm-generator-name>"`.
6. Ollama-down path: lane skips + re-queues, promotion unaffected,
   heuristic fallback serves responses.
7. Eval (OQ-3) demonstrating LLM summary > heuristic before the flag
   defaults on.
