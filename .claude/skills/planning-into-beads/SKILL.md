---
name: planning-into-beads
description: Use when the user wants to plan an epic for orchestrated execution - converts a brainstorm/design into a beads epic with a tagged DAG of tasks, each with testable acceptance criteria and a concrete definition of done. Refuses to commit to beads without explicit human approval of a preview block.
---

<EXTREMELY-IMPORTANT>
This skill is RIGID. The five-stage flow is the contract. You may not skip stages,
condense them, or "just create the tasks first and refine later."

Beads is touched ONLY in stage 5, and ONLY after the human has explicitly approved
the preview block.

If you find yourself wanting to call `bd create` before stage 5, STOP - that is the
exact failure mode this skill exists to prevent. Auto-committing planning output is
how task graphs rot.
</EXTREMELY-IMPORTANT>

## When to invoke

- User asks to plan an epic ("plan a feature", "decompose this design", "/plan ...")
- Brainstorming has already produced a problem statement and chosen approach
- User wants to refine an existing `requires_planning: true` task (`/plan --refine bd-x.N`)

If brainstorming has not happened yet, invoke the `brainstorming` skill first. This
skill assumes the design conversation is done.

## Modes

### New epic (default)

Stages 1–5 below. Produces one new epic with N tasks, deps, and tags.

### Refine (`/plan --refine bd-x.N`)

The named task is `requires_planning: true` (typically created from a worker
proposal). Skip stage 1 - the design doc is inherited from the parent epic. Run
stages 2–5 to decompose the task into properly-specified children. The original
`requires_planning` task is closed as superseded once children are created.

## The five-stage flow

```
[1] Synthesize design doc       (markdown, attached to epic, NOT shredded)
            ↓
[2] Decompose into candidates   (flat list in conversation, no graph yet)
            ↓
[3] Quality bar                 (mechanical checklist; iterate until pass)
            ↓
[4] Map deps + assign tags      (vocabulary check, edge justifications)
            ↓
[5] Preview + human checkpoint  (atomic commit on approval, or revise)
```

Stages 1–4 happen entirely in the conversation. Beads is only touched in stage 5.

---

### Stage 1 - Synthesize design doc

**Inputs.** Brainstorming output, problem statement, agreed approach.

**Output.** A compact markdown document (≤ 1 page) with these sections:

- **Problem** - why this work is being done
- **Approach** - chosen direction; alternatives mentioned briefly
- **Goals** - what success looks like at the epic level (different from per-task DoD)
- **Out of scope** - explicit list; prevents scope creep during execution
- **Risks / open questions**
- **Constraints** - performance, compatibility, deadlines, etc.

**Storage.** Three paths, pick the right one:

1. **Inline in epic body.** If the doc is short (≤ 1 page) and self-contained.
2. **`bd remember` with ID referenced from epic body.** If the doc is longer than fits comfortably in a task body but is *unique to this epic*.
3. **External doc with stage-1 extract inlined.** If the work belongs to a larger pre-existing design document (e.g., a project-level design doc covering multiple epics), inline a compact extract - Problem / Approach / Goals / Out of scope - into the epic body, and reference the larger doc by path. The extract is the single source of truth for *this* epic; the larger doc is context.

The doc (or its extract) is **canonical**; tasks reference it. Do NOT shred it into
task bodies - workers will not have the brainstorming context, and this doc is how
the "why" survives downstream.

---

### Stage 2 - Decompose into candidates

Produce a flat list in the conversation, NOT yet structured as a graph. For each
candidate:

- Imperative title
- One-line summary

Goal: get the *set* right before getting the structure right. Listing first,
structuring second prevents premature dependency commitments.

---

### Stage 3 - Quality bar (mechanical checklist)

Every candidate must pass each line. Failures get fixed before stage 4.

- [ ] Title is imperative, scoped, fits a PR (≈ 1–3 days work)
- [ ] Body has **Why** + **Scope boundary** (what is explicitly out) + design-doc reference
- [ ] ≥ 1 acceptance criterion, each testable (mentions specific behavior, file, command, or metric)
- [ ] Definition of Done is concrete (which tests pass, which docs updated, which checks ran)
- [ ] `estimated_scope` set: `small` / `medium` / `large`
- [ ] Tags: ≥ 1 `area:*` and ≥ 1 `kind:*` (existing vocabulary or proposed)
- [ ] Body < 300 words
- [ ] Self-contained - a worker reading body + design-doc ref can start without conversation context

**Split-or-rewrite heuristics.** Apply each; if any triggers, fix before stage 4:

- AC count > 3 → almost always two tasks fused. Split.
- Title contains "and" → almost always two tasks. Split.
- Cannot write an AC without "eventually" / "later" → vague or wrong scope. Rewrite.
- Two candidates touch >80% of same files → fold into one.
- Candidate is < ~30 minutes of work → fold into parent.

**After any fold or drop, renumber.** The preview block shows tasks numbered
contiguously (`.1, .2, .3, .4`) regardless of which candidates were dropped during
this stage. Gaps (`.1, .2, .4, .5`) look like a bug in the preview even when they're
not - beads assigns real IDs at commit, so the preview's numbering is purely
cosmetic; keep it tidy.

**The hardest discipline.** ACs describe **externally observable behavior**, not
implementation. "Service signs and verifies RS256 tokens against rotated keys" is
observable. "Service uses jose-go library" is not - it's an implementation choice
the worker should be free to make or revise.

A test for each AC: could a different implementation also satisfy it? If no, you
are over-specifying. Rewrite.

---

### Stage 4 - Dependencies and tags

#### Dependency rules (strict)

- Add edge `B → A` ONLY when B *literally cannot start* until A is done.
- **Each edge MUST have a one-sentence justification** ("B's code calls a function A creates"). If you cannot write the sentence without hand-waving, drop the edge.
- "Better to do A first" → not a dep. (Document as ordering hint in the epic body if it matters.)
- Resource contention → not a dep. (Goes in orchestrator's serialization rules instead.)
- Tag overlap → not a dep.
- "Just in case" → not a dep. Phantom edges silently kill parallelism.

**Considered-but-rejected edges.** By default these live in the conversation only - they are deliberation, not decision, and beads records decisions. One carve-out: if a rejected edge is the kind a worker might later re-propose via `cross_cutting` (e.g., "A and B touch the same module so they probably should have a dep"), file the rejection as a comment on the **epic** with tag `planning:rejection`. This gives the orchestrator a place to look when a worker proposes something already considered. Use sparingly - most rejections are forgettable.

#### Tagging discipline

1. Run `bd tag list` first AND **output the resulting vocabulary in the conversation** so the human can see what you are matching against. Silent vocabulary lookup is not enough - drift is invisible if the matching step is hidden.
2. For each task, propose tags from existing vocabulary first.
3. Only invent a new tag when no existing tag fits.
4. New tags go in a separate "tag proposals" block requiring human promotion.
5. Never use a tag as a dependency edge stand-in.

#### Tag namespaces (controlled vocabulary)

| Namespace | Purpose | Examples |
|---|---|---|
| `area:*` | Domain | `area:auth`, `area:billing`, `area:ingest` |
| `surface:*` | Boundary | `surface:public-api`, `surface:internal`, `surface:cli`, `surface:db-schema` |
| `touches:*` | Systems involved | `touches:migrations`, `touches:cache`, `touches:queue` |
| `risk:*` | Hazard signals | `risk:breaking-change`, `risk:perf-sensitive`, `risk:security-sensitive` |
| `kind:*` | Work type | `kind:feature`, `kind:refactor`, `kind:bugfix`, `kind:test`, `kind:docs` |

---

### Stage 5 - Preview and human checkpoint

Before ANY `bd` write happens, produce a single preview block to the user. Wait for
explicit approval. On reject → return to stage 2 with notes. On approve → atomic
commit (next section).

**Preview format:**

```
PROPOSED EPIC
  bd-NEW: "<title>"
  Tags: [area:..., ...]
  Branch: epic-bd-<NEW>-<slug>
  Design doc: <inline | bd remember id>
  max_tasks: <int>          (default 30; tunable)
  Goals:
    - ...
  Out of scope:
    - ...

PROPOSED TASKS (<N>)
  bd-NEW.1 [<scope>, <tags>]
    <title>
    AC1: <testable behavior>
    AC2: <testable behavior>
    DoD: <concrete checks>

  bd-NEW.2 [<scope>, <tags>]
    ...

PROPOSED DEPS
  bd-NEW.2 → bd-NEW.1   ("<one-sentence justification>")
  ...

PROPOSED NEW TAGS
  <namespace>:<value>    used in: bd-NEW.X, bd-NEW.Y     - promote? [y/n]

QUALITY GATE
  ✓ All tasks have ≥1 testable AC
  ✓ All tasks have estimated_scope set
  ✓ Total scope: <breakdown>
  ✓ No "and" in titles
  ✓ All edges have one-sentence justification
  ⚠ <warning>      (warnings do not block)
  ✗ <failure>      (failures block; fix before commit)

Approve? [y / edit / reject]
```

The Quality Gate block is computed by the skill from the candidates, not narrated.
Warnings (⚠) don't block; failures (✗) do.

---

## Atomic commit (stage 5 on approval)

Order, executed as one batch:

1. `bd create` epic with title, body (or design-doc ref), tags, `out_of_scope`, `max_tasks`
2. `bd update <epic-id> --metadata branch=epic-bd-<id>-<slug>`
3. `bd create` each task parented to epic (with title, body, ACs, DoD, scope, tags)
4. `bd dep add` for each edge (with the justification as the comment)
5. `bd tag promote` for any approved new tags
6. `bd remember` the design doc if not inlined into the epic body

If any step fails, the whole batch rolls back. If your beads version exposes
transactions (`bd transaction begin/commit`), use them. Otherwise simulate by
collecting all planned mutations as an ordered list with rollback IDs and applying
explicitly.

After commit: emit ONE summary line and stop.

```
Created bd-<id> with <N> tasks, <M> deps, <K> new tags. Branch: epic-bd-<id>-<slug>.
```

Nothing else. The graph is the artifact.

## Slug generation

`epic-bd-<id>-<slug>` where:

- `<id>` is the bd-id beads assigns at create time
- `<slug>` is derived from the epic title: lowercase, replace non-alphanumeric runs with `-`, trim leading/trailing `-`, cap at 40 characters

The slug is recorded in epic metadata (step 2 of atomic commit) so anything else
that needs the branch name reads it from beads - single source of truth.

## Refine mode specifics (`/plan --refine bd-x.N`)

When invoked with `--refine`:

1. Read the target task: `bd show bd-x.N --json`. Verify `requires_planning: true`.
2. Skip stage 1 - design doc is inherited from the parent epic.
3. Run stages 2–4 against the target task's title and body as the problem statement.
4. The preview block in stage 5 is scoped to the new children, not a new epic.
5. Atomic commit order:
   - `bd create` each child task parented to the original epic (NOT to bd-x.N)
   - `bd dep add` for inter-child edges and any edges into bd-x.N's existing dependents
   - `bd tag promote` for approved new tags
   - `bd close bd-x.N` with `superseded_by: [bd-x.N.children...]` in the message
6. Summary line:

```
Refined bd-x.N into <N> tasks. Closed bd-x.N as superseded.
```

### Worked example (refine)

Input - a `requires_planning: true` task created from a worker proposal:

```
bd-x.42  [requires_planning: true, area:perf]
  Title: "Add caching layer"
  Body: "bd-x.7 worker observed repeated DB hits for the user-profile lookup;
         proposes a cache layer in front of the profile service."
```

Output of `/plan --refine bd-x.42` - three children, original closed as superseded:

```
bd-x.42.1 [small, area:perf, kind:feature]
  Title: "Define cache interface in profile service"
  AC: interface declared at internal/profile/cache.go; no implementation yet
  DoD: compiles; no callers reach into internals

bd-x.42.2 [medium, area:perf, kind:feature]
  Title: "Wire cache interface into profile lookup path"
  AC: profile lookup consults cache before DB; cache miss falls through to DB
  AC: cache hit/miss observable via existing metrics
  DoD: integration test passes with in-memory cache fake

bd-x.42.3 [small, area:perf, kind:test]
  Title: "Add eviction-policy tests for cache interface"
  AC: tests cover TTL expiry and LRU eviction against the interface fake
  DoD: tests pass; no live cache backend required

DEPS
  bd-x.42.2 → bd-x.42.1   ("wiring needs the interface to exist")
  bd-x.42.3 → bd-x.42.1   ("tests target the interface")

CLOSURE
  bd close bd-x.42 --message "superseded_by: [bd-x.42.1, bd-x.42.2, bd-x.42.3]"
```

Note: children are parented to the original epic that contained bd-x.42, NOT to bd-x.42 itself. The `requires_planning` task was a placeholder; it does not become a parent.

## Red flags

| Thought | Reality |
|---|---|
| "I'll just create the tasks now and refine later" | No. Atomic commit only after human review. |
| "This task has 6 ACs but they're all related" | It's ≥ 2 tasks. Split. |
| "Better add a dep here just in case" | No. Edges only with one-sentence justification. |
| "I'll invent a new tag for this" | `bd tag list` first. Reuse beats invent. |
| "Workers can read the brainstorm" | They cannot. Body needs Why + Scope boundary + design-doc ref. |
| "AC: 'code is clean' / 'performance is good'" | Untestable. Name the test, file, or metric. |
| "AC: 'use jose-go for signing'" | Implementation, not behavior. Workers choose how. ACs describe what is observable. |
| "I'll add a 'cleanup' task for leftovers" | No catch-all tasks. Scope it or drop it. |
| "Brainstorm covered enough; skip the design doc" | The doc is the substrate the epic stands on. Workers reference it. Write it. |
| "I can decompose into 20 tasks for parallelism" | Coordination overhead beats parallelism past ~7. Aim for 3–7 tasks per epic. |
| "The skill is overkill for a small change" | Then it's 1 task with 1 AC and a 1-line body. The skill still applies. |
| "I'll commit the epic now and add deps later" | No. Deps are part of the atomic commit. Partial graphs are graph corruption. |
| "I'll skip stage 5 and just describe what I'm about to do" | The preview IS the human checkpoint. There is no other gate. |

## One trap

The seductive failure here is **the planner doing the implementation work in its
own head before writing the task** - ACs phrased as "implement X using approach Y"
instead of behavioral specifications.

This robs workers of the freedom to find a better approach, and makes ACs
untestable (you cannot test that code "uses approach Y," only that it behaves a
certain way).

Discipline: **ACs describe externally observable behavior. Implementation belongs
to the worker.**

## References

- Design doc: `docs/beads-orchestration-design.md`
- Upstream skill (run before this): `skills/brainstorming/SKILL.md`
- Companion skills (run after this): `skills/orchestrating-from-beads/SKILL.md`, `skills/accepting-epic-from-beads/SKILL.md`
