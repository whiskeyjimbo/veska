---
id: SOLO-11
title: "Pipelines — Save, Promotion, Review"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-04, SOLO-05, SOLO-08, SOLO-10]
verified: true
verified_date: "2026-05-16"
---

# SOLO-11 — Pipelines

Three pipelines run in the daemon. The save pipeline keeps staging
fresh. The promotion pipeline runs synchronous structural checks at
commit. The review pipeline runs LLM passes in the background and
is off by default. Auto-link, suppressions, revalidation, and the
merge-gate compute on top of those three.

This file is the whole orchestration story. There is no
"flavor A" / "flavor B"; there is no worker tier; there is no PR
webhook. Every pipeline runs as goroutines inside the one daemon
process.

## 1. Save pipeline

Triggered by fsnotify on every file write inside an indexed repo.

```
fsnotify event
  -> debounce [watcher].debounce (default 200ms; coalesces rapid saves)
  -> tree-sitter reparse for the changed file
  -> diff against the prior staging entry
  -> update StagingArea (in-memory map, repo-scoped)
  -> emit a "staging.updated" channel signal for any MCP readers
```

Save writes never touch SQLite. Staging is volatile by design: it
is lost on daemon restart, which is fine because the editor is the
source of truth for unsaved files. MCP queries read
staging-overlay-on-promoted (SOLO-08 §5).

Latency budget: see SOLO-13 §3.6. The intent is "imperceptible" —
the save pipeline must finish before the user notices the
keystroke. M1 measures.

**Large-file fallback.** Files above
`[save].large_file_threshold_loc` (DEFAULT 1500) reparse on a
background goroutine instead of blocking the save handler.
Staging continues to serve the prior good entry (or promoted if
none exists) and MCP reads stamp
`degraded_reasons: ["staging_reparsing:<path>"]` until the
reparse commits. Editor responsiveness is preserved at the cost
of save→staging freshness on the largest files. SOLO-13 §3.1b
covers the budget and the rationale; we do not pre-author an
incremental-parse plan today.

### 1.1 Coalescing

Saves arrive in bursts (editor format-on-save, multi-cursor edits,
auto-save tick). The debounce is per-file: a new save event resets
the timer; the reparse fires once the file has been quiet for
`[watcher].debounce` (default 200ms; CONFIG-SURFACE). The save
budget (SOLO-13 §3.6) starts when the debounce fires, not when
the fsnotify event arrives — debounce is a coalescing knob, not
part of the latency budget.

### 1.2 The staging-on-promoted read merge

MCP read tools that declare `Reads staging: yes` (SOLO-09 §3)
return a view that combines `StagingArea` with the promoted
SQLite state. The merge rule is normative — every read tool
follows it identically.

**Per-file overlay, not per-row.** Staging is keyed by
`(repo_id, file_path)`. When a read needs nodes/edges scoped to
a file, the resolver looks up that file's staging entry first.
Three cases:

| Staging entry | Promoted state | Rows returned |
|---|---|---|
| absent (file is clean) | any | promoted rows verbatim |
| present (file is dirty in editor) | any | **staging rows replace all promoted rows for that file_path on the active branch**; promoted rows for the same file_path are excluded entirely |
| present, marked deleted in staging (file removed in editor since last promotion) | rows present | no rows for that file_path |

The replace-all-by-file rule avoids row-level merging — a partially
overlapping stale node/edge from promoted cannot leak into the
view of a dirty file. Any read whose answer would draw nodes or
edges from a file with both staging and promoted rows uses staging
only for that file.

**Cross-file queries** (a `CALLS` edge from a clean file to a
dirty file's symbol) are resolved by walking the file boundary:
the source row comes from promoted, the target's *file* is
checked for a staging entry, and the target row comes from
staging if the entry is present. Edge endpoints can therefore
mix promoted and staged rows in a single response; what cannot
happen is a single file contributing both promoted and staged
rows.

**Worked example.** `eng_get_call_chain` starting at `f` in
`a.go` (clean), depth 2:

| Step | File | Source |
|---|---|---|
| `f` | `a.go` (clean) | promoted (no staging entry) |
| `f` calls `g` in `b.go` (dirty) | `b.go` | staging-only — promoted rows for `b.go` are excluded; if `g` no longer exists in staging, the edge dead-ends here |
| `g` calls `h` in `c.go` (clean) | `c.go` | promoted |

The traversal mixes rows from staging and promoted across the
*file* boundary; within `b.go` only staging contributes.

**Cross-repo + staging.** The cross-repo resolver chain (§9)
reads `nodes WHERE branch = R.active_branch` from the **target
repo's promoted state**. It does not consult the target repo's
staging. An agent calling `eng_get_call_chain` whose traversal
crosses into a target repo with unpromoted edits sees the target
repo's last-promoted view of those edits. This is intentional:
cross-repo answers always reflect a promoted state (SOLO-04 §5.4
invariant 2) so the MCP envelope's `as_of` tuple is meaningful.
The user sees the lag if their target repo has uncommitted
work; `veska doctor` reports the per-repo
`active_branch.uncommitted_files` count so the staleness is
visible.

**Conflict cases.** Two saves to the same file produce one
staging entry — the latest debounced parse wins. Saves to
different files don't conflict by construction. A `git checkout`
mid-edit fires the post-checkout hook (§1.3): staging is
dropped, the watcher reparses the new working tree, and reads
stamp `degraded_reasons: ["staging_recovering"]` until the
reparse completes.

**The active branch.** Staging is implicitly scoped to the
repo's `active_branch`. A read against a non-active branch sees
promoted-only — staging contributes nothing because it represents
*the editor's current state*, which lives on the checked-out
branch. Tools that take a `branch` arg explicitly different from
`active_branch` set `included_staging: false` on the response.

**No row-level diff merge.** There is no "edge in promoted,
contradicted by staging" three-way merge. The file is the
unit. This keeps the read path explainable and the test surface
small; it costs precision in the rare case where one file's
parse fails halfway and partial staging is rejected (SOLO-11 §8
"save reparse panic" handling — the file stays at last-good
staging or empty).

### 1.3 Branch-switch quiescence (post-checkout hook)

`git checkout <branch>` fires the `post-checkout` hook installed
by `veska repo add`. The hook calls the daemon's
`PostCheckout` RPC over `cli.sock`. The daemon's response:

1. **Bump the staging generation counter.** `StagingArea`
   carries a monotonic `generation uint64`. Every staging write
   tags itself with the generation that was current at the time
   the parse started. The branch-switch handler increments
   `generation` first, atomically. Any in-flight parse from the
   prior branch (a save event that fired moments before the
   checkout) commits its result against its own generation; the
   staging API rejects writes whose generation is older than
   the current. Stale writes are logged and dropped, not merged
   into the new-branch staging.
2. Pause the watcher's emit channel (events still queue at the
   kernel; they don't reach the parser).
3. Drop the repo's `StagingArea` entries — the editor's
   in-flight edits don't survive a branch change.
4. Update `repos.active_branch` to the new branch.
5. Re-walk the working tree's tracked files and seed staging
   from a fresh tree-sitter parse (the same path as a cold
   scan, but only producing staging rows, not promoted writes).
   Each parse tags writes with the new generation.
6. Resume watcher emit. New saves land normally.

The generation counter eliminates the save-vs-promote race during
branch switch: an in-flight parse from branch A can never
commit to branch B's staging, because the rejection is checked
at staging-write time against the counter, not against any
implicit timing assumption. The counter is in-memory only;
restart drops staging entirely (SOLO-03 §5.3).

While quiescence runs (steps 2–4), MCP read tools that read
staging stamp `degraded_reasons: ["staging_recovering"]` and
serve promoted-only views scoped to the new branch. The window is
seconds for typical repos; large refactor branches with many
dirty files are slower and the doctor surface reports the state.

The post-checkout hook is a no-op during `git rebase`, `git
merge --continue`, `git cherry-pick`, and `git bisect` — those
operations fire post-checkout transitions Git itself treats as
internal. §2.3 covers the promotion-pipeline side of the same story.

### 1.4 What save does NOT do

- Does not run the secrets scanner. Secrets-scanning the staging
  area on every keystroke is expensive and noisy.
- Does not run vuln-scan. Vuln scans key off dependency edges,
  which only change at commit.
- Does not score auto-link. Findings come from promotion and review,
  not save.
- Does not write findings. There are no findings until promotion.

## 2. Promotion pipeline

Triggered by `git commit` (via the post-commit hook running
`veska hook-runner post-commit`) or by `veska promote` for headless
batch runs. Runs synchronously inside the promotion SQL transaction or
the very next step before the hook returns.

```
post-commit hook fires
  -> daemon receives Promote RPC over the Unix socket
  -> BEGIN IMMEDIATE
       promote staging nodes/edges to nodes/edges tables
       run synchronous checks (§2.1)
       upsert findings produced by checks
       enqueue post_promotion_queue rows for async work
     COMMIT
  -> hook returns to git
```

Budget: see SOLO-13 §3.1, "Post-commit hook return p95". The
budget splits two cases — typical commit vs. refactor commit —
because synchronous structural checks (§2.1) scale with commit
size. Both budgets are unmeasured at the time of writing; M1
measures.

### 2.1 What runs synchronously

Four checks. All deterministic. All cheap.

| Check | Input | Output |
|---|---|---|
| Dead-code | Edge graph diff vs. promoted | Findings: `unreachable_symbol`, `dangling_import` |
| Secrets-scan | Diff hunks (changed lines only) | Findings: `secret_leak` with rule + redacted snippet |
| Vuln-scan | Dependency edges, against on-disk cache | Findings: `vuln` with advisory ID, package, range |
| Contract-drift | Public API edges vs. registered contracts | Findings: `contract_drift` with breaking-vs-additive class |

These run inline because they are deterministic and bounded. None
of them call out to the network at promotion time; vuln-scan reads the
cache file (SOLO-05 §VulnSource), which a background goroutine
refreshes on a configurable interval.

A check failure does not abort the promotion. Findings are advisory.
The `BEGIN IMMEDIATE` either commits cleanly or rolls back; either
way the user gets a finite hook return.

### 2.2 Async drain (promotion post-promotion queue)

The promotion transaction enqueues five kinds of work in `post_promotion_queue`:
`embed`, `auto_link`, `revalidate`, `wiki`, and (when enabled) `review`.
The `embed`, `auto_link`, and `revalidate` kinds are enqueued one row
per touched file; `wiki` is enqueued as a single repo-scoped row per
promotion (the wiki lane regenerates the whole `hot_zone` +
`entry_points` surface, so per-file rows would be redundant).
One goroutine per kind drains the table at 250ms poll. Goroutines
are independent: a stuck embedder does not block auto-link.

When a row exhausts retries (3 attempts), the error is
preserved and the row stays `state = failed`. `veska doctor
post-promotion-queue` lists every failed row; the user retries via
`veska doctor post-promotion-queue retry [--kind=K] [--seq=N]`, which moves
matching rows back to `pending`. The `review` kind also emits a
sticky `review-pipeline-failure` finding (rule
`review-pipeline-failure`, severity `high`,
`source_layer = quality`); closing that finding through the
human-action gate flips the row to `done` (ADR-S0004). This is
distinct from the per-commit token-cap finding emitted by §3.1
(`review-pipeline-budget-exceeded`, severity `medium`) — the
budget-exceeded finding is informational and non-sticky, the
pipeline-failure finding is sticky and human-action-gated.

### 2.3 Rebase, merge, cherry-pick, bisect

Git operations that rewrite or replay history fire post-commit
hooks at machine cadence — a 10-commit rebase produces ten
post-commit invocations in seconds. Promoting every one of them
would chew through SQLite write transactions, generate ten
embed batches per touched node, and (in the rebase case) fire
`ErrPromotionDivergent` immediately because each replay throws away
the prior `last_promoted_sha`. The hook handles this by detecting
the operation in progress and short-circuiting.

**Detection.** Before doing any work, `veska hook-runner
post-commit` checks the working tree for the canonical Git
markers:

| File / dir | Operation in progress |
|---|---|
| `.git/rebase-merge/` or `.git/rebase-apply/` | rebase (interactive or non-interactive) |
| `.git/MERGE_HEAD` | merge (interrupted; pre-`commit`) |
| `.git/CHERRY_PICK_HEAD` | cherry-pick |
| `.git/REVERT_HEAD` | revert |
| `.git/BISECT_LOG` | bisect |
| `.git/sequencer/` | multi-step pick/revert sequence (`git revert -n`, multi-commit cherry-pick) |
| `.git/AM` or `.git/rebase-apply/applying` | mailbox apply (`git am`) |

**Worktree handling.** `git worktree add <path>` creates an
auxiliary working tree with its own `$GIT_DIR` (`<repo>/.git/worktrees/<name>/`).
Hooks in the auxiliary tree run with `$GIT_DIR` pointing at the
worktree's gitdir, not the main repo's `.git/`. The marker
checks above honour `$GIT_DIR` (the hook runner reads markers
relative to the env var, not the working tree root) so a rebase
in worktree A does not block a normal commit in the main
checkout. The daemon registers each worktree as a separate repo
via `veska repo add <worktree_path>` if the user wants both
indexed; otherwise edits in the unregistered worktree are
invisible to MCP, which is consistent with `veska repo add`
being explicit.

When any marker is present, the hook returns 0 immediately
without contacting the daemon. No promotion, no post-promotion queue enqueue, no
`last_promoted_sha` advance. Output: nothing on success; one
debug-level log line if `VESKA_LOG_LEVEL=debug`.

**Catch-up at end of operation.** When the operation finishes
(rebase completes, merge `--continue` succeeds, bisect ends),
the *next* post-commit invocation runs without a marker and
proceeds normally. At that point the daemon's startup-resync
path is irrelevant (the daemon was up); the promotion pipeline
catches up by walking `git log <last_promoted_sha>..HEAD --reverse`
and replaying each commit through the same SQL pipeline a live
promotion uses. This is the same routine the §5.7 startup-resync
runs; the post-commit hook calls it explicitly when it detects
`HEAD` has moved past `last_promoted_sha` by more than one
commit.

**`ErrPromotionDivergent` is not fired during a rebase replay.** A
rebase changes commit SHAs, so `last_promoted_sha` will not be
reachable from `HEAD` after the rebase. The catch-up routine
treats this case the same as the startup-resync divergent path
(SOLO-03 §5.7): log `ErrPromotionDivergent`, fall back to a fresh
parse of the working tree, set `last_promoted_sha = HEAD`. The
distinction from a "real" divergent case (force-push from
elsewhere) is impossible to make from the daemon's view — both
are recorded; the user investigates if they didn't intend a
rewrite.

**`git commit --amend` falls through the divergent-promotion path.**
Amend has no `.git/` marker visible to the post-commit hook, so
the hook runs normally. The original commit's SHA is now
unreachable from `HEAD` (amend rewrote it), so `last_promoted_sha`
is not in `git log HEAD --first-parent`. The catch-up routine
treats this exactly like the rebase divergent case above: log
`ErrPromotionDivergent`, fall back to a fresh parse of the working
tree, set `last_promoted_sha = HEAD`. The orphaned prior commit's
nodes/edges remain in SQLite attributed to the dead SHA — they
are not actively pruned (the audit log still references them);
`veska gc --branches` (SOLO-08 §6) is the eventual collector,
matching how orphaned-branch findings are swept on a normal
divergent promotion. Frequent amend
loops therefore produce graph bloat proportional to the amend
count; users with heavy amend workflows should expect
`veska doctor storage` to show drift between graph node count
and reachable-from-`HEAD` node count.

**Bisect is read-only as far as Engram is concerned.** Each
`git bisect` step does a `checkout` (handled by §1.3) but the
intermediate commits are not user authorship; promoting them
produces no useful state and pollutes the audit log. The
post-commit hook short-circuit covers this; the post-checkout
hook also short-circuits its quiescence path during a bisect to
avoid storming the watcher (`.git/BISECT_LOG` present →
post-checkout returns 0).

This shape mirrors the V1 hook behaviour and is intentional:
during in-progress Git operations Engram's view is stale, and
that is fine because no one is querying it during a rebase.

### 2.4 What promotion does NOT do

- Does not call the LLM. Review is a separate pipeline.
- Does not file tracker issues. Tracker integration is opt-in and
  fires from the review pipeline (or from explicit MCP tools).
- Does not block on embedding. Semantic search degrades to "no
  vector for this content_hash yet" with `degraded_reasons:
  ['embedding_pending']` until the embedder catches up.

## 3. Review pipeline

Off by default. When enabled, runs as goroutines after the promotion
transaction commits, draining `post_promotion_queue` rows of kind `review`.

```yaml
# ~/.veska/config.toml
[review]
enabled              = false      # default
specialties          = []         # e.g. ["security", "contract"]

# Hard-halt token ceilings. Counted across all specialties.
max_tokens_per_commit = 100000
max_tokens_per_day    = 500000

# USD ceilings come with hosted LLM providers when they ship.
# We ship only the local Ollama generator today (cost = 0);
# token caps are the meaningful control. Cap reset is local-
# midnight. Cap-tripped behavior: log + refuse new review work
# + resume on reset (SOLO-11 §3.1). Not surfaced as a Finding.
```

Review costs real time and CPU. Ships only the local Ollama
`LLMGenerator` (SOLO-05 §2.5); on a mid-sized model the
per-commit pass is **seconds to minutes**. Hosted providers
(Anthropic, OpenAI, Gemini, OpenAI-compatible) and the USD-cost
machinery they require come behind a future ADR + local
measurement. Ships with two specialties:

| Specialty | What it does | Default impl |
|---|---|---|
| `security` | Re-reads the diff with a security prompt; emits findings on suspect patterns the structural rules can't catch | `LLMGenerator` port, prompt in `internal/review/prompts/security.txt` |
| `contract` | Re-reads contract-drift hits from promotion; classifies as breaking-vs-additive with rationale | same port, different prompt |

Each specialty runs independently. Findings carry
`actor_id = "agent:<llm-generator-name>"` and `actor_kind = 'system'`
(the daemon owns the goroutine; the LLM is the attribution label,
not the gate input).
Findings show up in MCP via `eng_list_findings` once the goroutine
finishes.

### 3.1 Cost & latency

On a local CPU Ollama with a mid-sized generator model the
per-commit review pass is seconds-to-minutes per touched file.
Concrete budgets and the gate that turns them into measurements
live in SOLO-13 §3.6.

**Per-commit token cap.** If `max_tokens_per_commit` is exceeded
mid-commit, remaining specialties for that commit skip and a
`BudgetExceeded` finding is filed (rule
`review-pipeline-budget-exceeded`, severity `medium`,
`source_layer = quality`). The user sees it in the editor;
nothing silently drops. The next commit starts fresh.

**Daily token cap.** `max_tokens_per_day` is a hard halt on the
*pipeline*, not the commit. Wallet-style caps are not Findings —
making the user human-action-gate-close their own daily budget would run
an operational signal through the same machinery as a CVE. The
shape that fits a solo product:

1. The daemon stops dispatching new `review` rows from
   `post_promotion_queue`. In-flight jobs run to completion (their
   token spend is already counted).
2. The daemon writes one line to `audit.jsonl`:
   `{"tool":"review-pipeline","result":"paused: tokens_per_day cap reached","args":{"cap":N,"spent":N,"resets_at":"..."}}`.
   No Finding row, no human-action gate.
3. `veska doctor pipelines` surfaces the paused state, the
   measured spend, and the next reset time. That is how the user
   sees what happened; it is also where they raise the cap if
   they want to.
4. New `review` post-promotion queue rows queue normally but stay `pending` —
   they are not dropped. Promotion continues unaffected.
5. At the next reset boundary the pipeline resumes; the queued
   rows drain in FIFO order. No human action required for the
   resume.

**Reset boundary (timezone).** The cap window is anchored to a
fixed offset, not "local midnight" interpreted at check time
(laptops change timezones mid-flight; a SFO→LHR traveller would
otherwise see the cap reset 8 hours late). The daemon records
the host's IANA timezone on first daemon start in
`database_meta.review_cap_tz` and uses that zone for every
subsequent reset until the user runs `veska review reset-tz`
to relocate. `veska doctor pipelines` shows the active zone
and the next reset time as an absolute UTC instant.

USD caps come with hosted providers; today we track tokens only.
Current spend is visible via `veska doctor pipelines`.

### 3.2 No PR comment, no CI integration

The review pipeline writes findings into SQLite. That is the only
output channel. The editor reads them via MCP. Posting comments
to GitHub or Slack is not in scope. Forward findings yourself if
you want them elsewhere.

## 4. Auto-link

Every finding (from promotion or review) goes through auto-link once,
at write time. The result is persisted on the finding row.

The score function combines three signals against the open task
list (from the active `tracker` port):

| Signal | What it scores | Weight |
|---|---|---|
| Branch name match | Embedding similarity between branch name and task title | TBD: M3 calibration |
| Touched-symbol match | Whether the finding's anchor symbol appears in or is semantically near task descriptions | TBD: M3 calibration |
| Recent activity | Tasks the user has touched recently (via `task_set_active` or `bd update`) | TBD: M3 calibration |

```
confidence = clamp(0, 1, w_branch*branch + w_symbol*symbol + w_activity*activity)
```

| Confidence band | Action |
|---|---|
| `[T_high, 1.0]` | Auto-link target — but ship as **suggest only** until calibrated |
| `[T_low, T_high)` | Suggest — set `details.suggested_task` |
| `[0.0, T_low)` | No link |

**Calibration honesty.** The weights and thresholds (`T_high`,
`T_low`) are unset on purpose — they need real-workload tuning
before any finding is auto-linked silently. M3 epic m3.04.4
("FP measurement on fixture") is where the numbers come from;
the M3 close report records them. Until calibration ships, every
band emits as suggest. The config flag
`autolink.mode = "suggest" | "auto"` switches the high-band
behavior; default is `suggest`. **No illustrative numbers appear
in this section until they are measured** — readers anchor on
numbers they see, even ones labelled "placeholder."

### 4.1 Manual override

`~/.veska/<repo_id>/active_task` (a one-line file) pins the
active task. When set, auto-link returns the pinned task
regardless of score. `veska task bind <id>` writes it; `veska
task unbind` removes it.

### 4.2 Re-link on revalidation

The revalidation sweep (§6) does not re-score auto-link by
default. Auto-link is a write-time decision. The exception: when
revalidation re-opens a finding (e.g. a previously-resolved vuln
re-fires), it re-scores once on re-open.

## 5. Suppressions

A `Suppression` silences a finding for a stated reason. Stored
append-only in SQLite (SOLO-08 §3.2). The pipeline never deletes
prior suppression rows; new opinions are new rows.

### 5.1 Scopes

| Scope | What it silences |
|---|---|
| `finding` | Exactly one finding by ID |
| `symbol` | Every finding on a given `symbol_path` |
| `file` | Every finding under a file path |
| `repo` | Every finding in the repo (rare; reserved for noisy rules) |

Broader scopes require `actor_kind = 'human'`. The CLI sets it; an
agent over MCP cannot suppress at file or repo scope.

### 5.2 Conflict resolution

Two dimensions: hard-vs-soft (lifetime) and branch-scope
(specificity). The combined rule:

**branch-specific overrides branch-agnostic; hard overrides soft;
last-affirmative wins within a tier.**

- A "hard" suppression has `expires_at = NULL`. A "soft" suppression
  has a finite `expires_at`.
- A "branch-specific" suppression has `branch = <X>`. A
  "branch-agnostic" one has `branch = NULL`.
- For a finding on branch X, the resolver considers four tiers in
  order:
  1. Hard, branch-specific (`branch = X`, `expires_at = NULL`).
  2. Soft, branch-specific (`branch = X`, finite `expires_at`).
  3. Hard, branch-agnostic (`branch = NULL`, `expires_at = NULL`).
  4. Soft, branch-agnostic (`branch = NULL`, finite `expires_at`).
- The first tier with any non-revoked matching suppression
  decides. Within a tier, the most-recent non-revoked row wins.
- A revoke is a new row with the same target/branch and a
  `revoked = 1` marker; it does not delete the prior row.

The branch-specific-overrides-branch-agnostic rule lets the user
say "this finding is suppressed on every branch except this one
where I want to see it" by writing a soft branch-specific revoke
on top of a hard branch-agnostic suppression.

This is enough for one user. Multi-actor disagreement is not a
problem we have, because there is no second actor to disagree with;
the agent and the human writing through the same daemon either
agree (one suppresses, no one revokes) or the human revokes the
agent (which the human-action gate already requires `actor_kind = 'human'`
to do at HIGH severity).

### 5.3 Soft-suppression auto-revert

The revalidation sweep walks expired soft suppressions. Each
expired row gets a `revoked = 1` companion row written under the
daemon's `actor_id`. The underlying finding re-surfaces.

## 6. Revalidation sweep

A goroutine that runs hourly (configurable) and re-evaluates every
open finding against the current promoted graph **on the active
branch**. Findings are per-`(finding_id, branch)` (SOLO-04 §8.1),
so revalidation only touches rows for the active branch; other
branches' rows remain untouched until that branch is checked out
and the next sweep runs.

```
every <cadence>:
  snapshot the current promoted graph at the active branch
  for each finding where state = 'open' AND branch = active:
    if the anchor symbol's content_hash has not changed since detected_commit:
      bump details.last_checked; continue        // temporal short-circuit
    re-run the rule that produced it (cheap; usually O(1) graph lookup)
    if anchor symbol no longer exists:
      mark orphaned (state = 'open', details.orphaned = true)
    if rule no longer fires:
      transition to closed, IF allowed (§6.1)
    else:
      bump details.last_checked
  for each soft suppression where expires_at < now:
    write revoke row; underlying finding re-surfaces
```

The **temporal short-circuit** is an optimisation, not a separate
plugin: when a finding's anchor symbol has the same `content_hash`
it had at `detected_commit` (resolvable via the same graph state
`eng_get_node` reads), the rule's premise is unchanged and
the rule does not need to re-run. Full rule rerun is the fallback.
This closes the common "the file moved, the symbol didn't change"
case cheaply — most open findings on a typical branch hit this
path.

The sweep reads a single graph snapshot at the start. If the
branch advances mid-sweep, the sweep aborts and reschedules; this
keeps revalidation idempotent.

### 6.1 The HIGH-severity guard

Revalidation never auto-closes findings with `severity >= high`
without `actor_kind = 'human'` on the close. The sweep
proposes a close by writing a `revalidation_proposes_close`
finding-detail; the human confirms (via MCP `eng_close_finding`
or CLI `veska finding close`) and the finding closes. Without
confirmation, the proposed close stays as-is until the next
sweep, which re-confirms the proposal is still valid.

This is the single human-action-gate cross-cutter visible in the
pipelines. SOLO-10 owns the rule; this section calls it out
because revalidation is where it most often bites.

### 6.2 Performance budget

See SOLO-13 §3.6. The intent is "the sweep finishes well under
the cadence interval even on large finding sets." M2 measures.

## 7. Merge gate / preflight

A function the daemon evaluates and reports through the CLI and
the editor. It does not enforce anything by itself — it is a
function `branch -> {eligible: bool, reasons: [...]}`.

```
inputs:
  branch (head SHA, base SHA)
  open findings on the branch
  config thresholds (severity, required review specialties)

output:
  { eligible: bool, reasons: [GateFailure...] }
```

A branch is eligible when all of:

1. No `severity >= high` findings open without a non-revoked
   suppression.
2. Every required review specialty (per config) has completed
   for this commit.
3. No expired soft-suppressions still in effect.

### 7.1 Surfaces

| Surface | Use |
|---|---|
| `veska preflight` CLI | One-shot. Exits non-zero on ineligible. Wrap it in a `pre-push` hook if desired. |
| MCP `eng_preflight_status` | The editor or agent reads the same view. |
| `veska doctor preflight` | Human-friendly summary. |

That is the entire enforcement story. There is no GitHub status
check, no branch-protection wiring, no tracker "blocking issue".
The user (or their `pre-push` hook) is the enforcement.

### 7.2 Force override

`veska preflight --force` exits zero with a `forced: true` audit
entry. Findings stay; the gate just doesn't block. The audit log
records who forced and why (the `--reason` flag is required when
`--force` is used).

## 8. Failure isolation

Every pipeline step is a goroutine with its own bounded channel
and its own retry policy. A failure in one does not stall the
others.

| Failure | Behavior |
|---|---|
| Save reparse panics on a file | Log; mark file degraded; staging stays at last-good entry |
| Promotion SQL transaction fails | Hook returns non-zero; user retries the commit (or runs `veska promote --retry`) |
| Embedder goroutine crashes | Daemon restarts it; pending refs stay `pending`; semantic search degrades |
| Auto-link goroutine crashes | Findings stay unlinked; user can manually bind tasks |
| Revalidation overruns its cadence | Skip the next scheduled run; emit warning in `veska doctor` |
| Review goroutine times out | The specialty's row in `post_promotion_queue` retries; on final failure, finding `BudgetExceeded` is filed |

`veska doctor pipelines` reports queue depths, last successful
run, and last error per goroutine.

## 9. Cross-repo resolver chain

Stored edges are intra-graph (SOLO-04 §5.2). When the daemon holds
multiple registered repos (SOLO-03 §3.0), the resolver chain
synthesises cross-repo edges at **query time** during traversal
tools (`eng_get_call_chain`, `eng_get_blast_radius`,
`eng_get_diff_blast_radius`, `eng_find_owner` over a diff).

**Two persistent inputs, one read path.** Two kinds of source-
side state feed the resolver:

1. **`cross_repo_edge_stubs` rows** (SOLO-08 §3.1) — written at
   source-side promotion when the parser produces a cross-repo
   reference whose target lookup keys (`module_path`,
   `symbol_path`, `language`) are already known. These are the
   common case: an `import "github.com/org/sdk"` plus a call to
   `sdk.Foo` produces one stub at promotion time.
2. **Unresolved intra-graph edges** — edges with
   `confidence = unresolved` whose target the parser couldn't
   pin to a stub key (e.g. dynamic dispatch, language-level
   ambiguity). The resolver attempts to bind these
   speculatively at traversal time.

The resolver reads stubs first (cheap point lookup keyed on the
source `node_id`), then walks unresolved edges as a second pass.
Both code paths converge on the same target lookup against
`nodes`:

```
for each cross-repo candidate C reached during traversal:
  // C carries (src_node_id, kind, module_path, symbol_path, language).
  // Stubs supply C from cross_repo_edge_stubs rows; unresolved-edge
  // probing supplies C by inferring (module_path, symbol_path, language)
  // from the edge's target string.
  look up repos WHERE module_path = C.module_path
  for each candidate repo R:
    look up nodes WHERE repo_id = R.id
                   AND language = C.language
                   AND symbol_path = C.symbol_path
                   AND branch     = R.active_branch
    if hit:
      emit synthetic edge (cross_repo: true,
                           src_node_id:   C.src_node_id,
                           target_repo_id: R.id,
                           target_branch:  R.active_branch,
                           via:            "stub" | "unresolved_edge")
      if expand_cross_repo: enqueue R's node for further traversal
      else: stop traversal at R's node
```

`via` distinguishes the two source paths so the audit-log
`resolver` field (SOLO-08 §3.5.1) can record provenance. Stubs
are the strong-signal path; unresolved-edge probing is best-
effort and may emit synthetic edges that a future parser
improvement would have produced as stubs at promotion.

Properties:

1. **Indexed point lookup.** Each cross-repo hop is one indexed
   query against `nodes(repo_id, branch, symbol_path)`. SQLite
   handles these at hot-cache speeds; the budget for
   `repo: "*"` traversal is in SOLO-13 §3.4.
2. **Always current.** The lookup uses the target repo's
   `active_branch` and its current promoted state. There is no
   version-pinning, no per-source-commit snapshot of the target.
3. **One hop default.** Traversal stops at the cross-repo node
   unless the caller passes `expand_cross_repo: true`. This keeps
   blast-radius bounded across a working set of N repos.
4. **Silent miss when target unindexed.** If no registered repo
   matches the import path, the local edge stays
   `confidence = Unresolved` and no synthetic edge is emitted.
   The agent sees the unresolved edge in the same shape it would
   in single-repo mode.
5. **No invalidation work on target promotion.** Because nothing is
   stored, target-repo promotions do not trigger fanout. The next query
   sees the new state.

### 9.1 Resolver materialisation cache (gated, not pre-shipped)

If the SOLO-13 §3.4 query-time budget misses at M1, the
documented fallback is a materialisation cache rather than a
promotion of stubs into `edges`. This subsection is the design
of that fallback so M1 doesn't have to invent it under pressure;
the cache itself **does not ship at M1** unless OQ-S010 lands
yellow or red on the M1 bench.

**Schema:**

```sql
CREATE TABLE cross_repo_edge_cache (
    src_node_id          TEXT NOT NULL,
    src_branch           TEXT NOT NULL,
    target_repo_id       TEXT NOT NULL,
    target_active_branch TEXT NOT NULL,
    target_node_id       TEXT NOT NULL,
    kind                 TEXT NOT NULL,
    resolved_at          INTEGER NOT NULL,
    PRIMARY KEY (src_node_id, src_branch, target_repo_id, target_active_branch, kind)
);
```

The key includes the target's `active_branch` because the
resolver's output depends on which branch the target repo
currently has checked out (§5.4 invariant). A cache that omits
it serves stale targets across target-side `git checkout`.

**Invalidation rules** (in order of arrival):

1. **Target promotion.** When `repos.last_promoted_sha` advances for
   `target_repo_id`, every cache row with that
   `(target_repo_id, target_active_branch)` pair is dropped.
2. **Target branch switch.** When `repos.active_branch` changes
   for `target_repo_id`, every cache row with that
   `target_repo_id` is dropped (regardless of stored
   `target_active_branch` value, because the new active branch
   has no cache entries yet).
3. **Source promotion.** When `repos.last_promoted_sha` advances for
   the source repo, every cache row with that source `repo_id`
   is dropped (the source side may have new or removed
   cross-repo references).
4. **Cache age.** A cache row older than
   `[cross_repo].cache_ttl` (DEFAULT 1h) is treated as a miss
   and dropped on next access.

Invalidations 1–3 run inside the promotion transaction that triggers
them; the cache invalidation is part of the promotion SQL, not a
separate goroutine. This keeps "the cache is consistent with the
promoted state" a transactional property rather than a
best-effort promise.

**Read path.** The resolver looks up the cache first; on hit,
serves the cached `target_node_id`. On miss, runs the §9 chain,
inserts the result into the cache, and serves it.

**Why this is not a stored edge.** A row in `cross_repo_edge_cache`
is keyed on the *source* graph's identifiers and a *target* repo's
runtime branch state. The `Edge` aggregate's invariant — both
endpoints in the same graph — is not violated: the cache row is a
materialisation, dropped on any change to either endpoint's
underlying state. SOLO-04 §5.2 invariant 1 stays intact.

**Gate.** **OQ-S010** at M1 (sub-issue `m1.10.9-bench`) decides
whether the cache ships. Green: cache stays unwritten, §3.4
budget rows relabel `BUDGET (measured M1)`. Yellow or red: the
cache ADR is written against measured numbers and ships in M2.

## 10. Write serialization (promotion vs. MCP writes vs. embed)

The full rationale is **ADR-S0011**. The shape: three `*sql.DB`
handles to the same `veska.db` — `readDB` (unlimited), and two
single-writer pools `writeDB.hot` (promotion + MCP writes + non-embed
post-promotion queue transitions) and `writeDB.embed` (embed payloads only).
`MaxOpenConns=1` on each writer pool serializes **transaction
acquisition** at the pool: every transaction (`BEGIN IMMEDIATE`
through `COMMIT`) holds the connection exclusively, so
transactions on the same pool execute one at a time. Between
pools, SQLite's OS-level write lock arbitrates under
`BEGIN IMMEDIATE`; an embed transaction in flight on
`writeDB.embed` blocks a hot transaction's `BEGIN IMMEDIATE` on
`writeDB.hot` until the embed commits. WAL keeps reads off both
writer paths. Embed batches chunk at ≤256 rows
(`[embed].batch_size`); per-pool `busy_timeout` is
5000/30000/5000 ms (hot/embed/read).

**On the warm path, hot writes interleave at COMMIT
boundaries.** A short MCP write (close finding, set active
task) holds `writeDB.hot` for low milliseconds; if a refactor
promotion arrives in that window, its `BEGIN IMMEDIATE` waits at the
pool until the prior transaction commits, then runs. The 5s
hot-pool busy-timeout caps any pathological case (an external
`sqlite3` shell holding the database lock); under normal
single-process operation the timeout is never reached because
no in-process holder takes more than the embed-batch chunk
budget.

### 10.1 What MCP write callers see

Every MCP write tool documents:

- **Default behaviour.** Block until `writeDB.hot` allocates a
  connection and the transaction commits, or until the default
  deadline (`[mcp].write_max_wait_ms`) expires.
- **`max_wait_ms` argument.** Optional per-call override. Set
  as the call's context deadline.
- **`ErrBusy` semantics.** Returned when the deadline expires
  waiting for `writeDB.hot`. The error payload (SOLO-09 §4.6)
  carries `data.context.cause` set to `"seal_in_flight"` (promotion
  holds the connection — payload includes `promotion_id` and
  `eta_ms`) or `"pool_wait"` (other writes ahead — payload
  includes `wait_count`, `wait_duration_ms`, `eta_ms` from
  `sql.DBStats()`).

Defaults (DEFAULT; CONFIG-SURFACE.md `[mcp]`):

| Setting | Default | Notes |
|---|---|---|
| `write_max_wait_ms` | 3000 | Applies to every write tool unless the handler overrides. |
| Handler override for `eng_add_repo` / `eng_remove_repo` | 30000 | Cold-scan-bounded; coded into the handler. |

### 10.2 The promotion barrier

**One bit, one job: prevent post-Promotion MCP writes from queueing
ahead of the promotion at the hot pool.** The barrier is structurally
*not* a priority lane: no per-kind queue, no fairness scheduler,
no work-class metadata. It is the smallest mechanism that keeps
the SOLO-13 §3.1 typical-commit budget honest under agent write
bursts. ADR-S0011 records the rationale for keeping the rest of
the writer-pool design flat.

`veska promote` over the Unix socket opens a transaction on
`writeDB.hot` and runs the promotion SQL (SOLO-08 §5). It does not
bypass the pool, and it does not preempt an MCP write *already
holding* the connection.

The contract:

1. When the `Promote` RPC arrives, the daemon raises the barrier
   *before* it calls `writeDB.hot.BeginTx`. The barrier is one
   boolean (or one-element `chan struct{}`) per daemon — it is
   not a queue, not a priority lane, and not aware of work
   kinds. Refcounting is unnecessary because promotions do not stack:
   `git commit` is single-shot, and the rebase / merge /
   cherry-pick / bisect short-circuit (§2.3) collapses bulk
   replays into one promotion. **Replay catch-up serialisation.** The
   §2.3 catch-up path that replays `git log <last>..HEAD --reverse`
   inside one daemon-side routine processes commits in order and
   takes the barrier per-commit; a fresh user `git commit`
   arriving mid-replay queues at the barrier behind the in-flight
   replay promotion and runs after it. There is no overlap; "promotions do
   not stack" is preserved by serialising the catch-up routine
   itself.
2. Any MCP write tool that has **not yet acquired** `writeDB.hot`
   waits on the barrier (subject to its own `max_wait_ms`)
   before contending for the pool. New write tool requests block
   on the barrier; reads are unaffected.
3. MCP writes that **already hold** `writeDB.hot` when the promotion
   RPC arrives complete normally. The barrier never preempts an
   in-flight transaction; it only gates entrants.
4. The promotion then takes the connection in normal FIFO order
   (typically immediately, because step 2 has stopped new
   entrants and step 3's holder commits in low milliseconds),
   runs its transaction, and drops the barrier on commit *or*
   rollback. A panic in promotion still drops the barrier via
   `defer`.
5. While the barrier is up, MCP write tools that exceed
   `max_wait_ms` return `ErrBusy` with
   `data.context.cause = "seal_arriving"` (distinct from
   `"seal_in_flight"`, which means the promotion already holds the
   connection). The payload carries `promotion_id` and the same
   `eta_ms` shape as `seal_in_flight`.

The hot pool's 5s `busy_timeout` continues to cap pathological
cases (e.g., an external `sqlite3` shell holding the database
lock).

### 10.3 post-promotion queue drains and the two pools

Four of the five post-promotion queue `work_kind`s (`auto_link`,
`revalidate`, `review`, `wiki`) write findings/state via `writeDB.hot`:
their writes are short SQL and rare relative to promotions.

`embed` is the only kind routed to `writeDB.embed`. Each embed
drain iteration does two writes:

- The state transition (`pending → in_progress`, then
  `in_progress → done`) goes through `writeDB.hot` because it's
  a tiny SQL update on `post_promotion_queue` and we want it visible
  fast in `veska doctor post-promotion-queue`.
- The payload work — embedding-store insert plus `vec_nodes`
  upsert — goes through `writeDB.embed`.

Two small writes per embed row, on pools sized for them.

### 10.4 What this design does NOT include

- **Read-write transactions across the MCP boundary.** Every
  MCP write is a single transaction on `writeDB.hot`. There are
  no client-driven multi-statement transactions. If a write
  needs read state, it reads inside its own transaction.
- **Priority queues.** The hot pool is FIFO. The embed pool is
  serialized by SQLite under it. Two pools = two priorities;
  that is the entire priority surface.
- **Application-layer write coordination.** No
  `promotion_coordinator` goroutine; no `WriteOp` channel. The
  pipelines call repository `Save(ctx, *Entity)` methods
  directly (SOLO-04 §11); the adapters open transactions on the
  appropriate pool.
- **Cross-process write coordination.** The CLI shells out to
  the daemon over the Unix socket. There is no other process
  writing to `veska.db`. If the user opens `sqlite3 veska.db`
  externally and writes, that's their business and may corrupt
  state; we do not defend against it.

## 11. Configuration

One file: `~/.veska/config.toml`. The pipeline-relevant section:

```toml
[save]
debounce_ms = 100

[promotion]
checks = ["dead_code", "secrets", "vuln", "contract_drift"]

[review]
enabled               = false
specialties          = []
max_tokens_per_commit = 100000
max_tokens_per_day    = 500000
# USD caps come with hosted LLM providers when they ship.

[autolink]
mode = "suggest"            # "suggest" | "auto"
weights = { branch = 0.40, symbol = 0.40, activity = 0.20 }

[revalidation]
cadence = "1h"

[preflight]
severity_threshold = "high"
require_review_specialties = []
allow_force = true
```

Editing the config is a CLI action (`veska config edit`); the
daemon reloads on next pipeline-tick.
