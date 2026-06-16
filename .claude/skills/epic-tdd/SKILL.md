---
description: "Iterate through every ready issue in a Beads epic using a strict TDD loop: tests-first → implement → race-clean → vet/lint → concurrency review → fmt/fix → commit. Each issue runs as an isolated sub-agent; the orchestrator closes the bead and routes context forward."
arguments: [epic_id]
allowed-tools: [Bash, Read, Edit, Write, Agent]
---

# Epic TDD Loop

## Pre-flight

1. `bd show $epic_id` - confirm epic, get title.
2. `bd list --parent $epic_id --status=open --limit 500` + `bd blocked` - collect ready issues in dependency order.
3. Print `Epic $epic_id - N issues` + ordered ID list.

## Per-Issue Orchestration (serial, blockers first)

**NEVER dispatch multiple sub-agents in parallel. DISPATCH 1 agent for One issue at a time - wait for the Result Block before starting the next.**

### Step 0 - Clarify (orchestrator, interactive)

`bd show <issue_id>`. Read relevant code. **Pause and surface block** if any: description < 2 sentences of real behaviour; can't locate the change in the codebase; criteria are structural not behavioural; two interpretations would produce different code; touches an undescribed domain concept.

```
┌─ Clarifying <issue_id>: <title> ─────────────
│  My read: <1–2 sentences on what and where>
│  1. <specific question>
│  2. <if applicable>
│  Say "proceed" if my read is correct.
└───────────────────────────────────────────────
```

Two sharp questions > five fuzzy ones. After response: `bd update <issue_id> --description="<original + clarifications>"`

### Step 1 - Dispatch Sub-agent (one at a time - never parallel, never `run_in_background`)

Spawn **one** Agent with the template below, filling in `$issue_id`, the full `bd show` output, and (if relevant) a trimmed `## Prior Context` block from the previous result. Wait for the Result Block before moving to the next issue.

**Sub-agent prompt:**
> You are implementing a single beads issue using strict TDD. The orchestrator closes the bead - do NOT call `bd close`.
>
> **Issue:** $issue_id
> **Details:**
> ```
> $bd_show_output
> ```
> $prior_context_block
>
> Work directory: /home/jrose/src/engram/solov2. Follow Sub-agent Steps 1–8. Return only the Result Block.

### Step 2 - Close Bead (orchestrator)

- `✓ COMPLETE` → `bd close <issue_id> --reason="DoD verified: <one line>"`
- `✗ BLOCKED` → `bd update <issue_id> --status=open`

### Step 3 - Report & Route

Print Result Block verbatim. Extract **Context Hand-off**; carry forward only entries relevant to the next issue (new shared types, changed interfaces, hot files, patterns to follow). Discard issue-specific detail.

---

## Sub-agent Steps 1–8

**Step 1 - Claim & parse DoD**
```bash
bd update <issue_id> --claim && bd show <issue_id>
```
Extract DoD/acceptance criteria as a numbered checklist of observable runtime behaviour (not code structure).

**Step 2 - Write failing tests**
Write Go tests for each criterion. Confirm they fail (single run, suppress output). Tests + implementation ship in one commit.

**Step 3 - Implement (minimum viable)**
Only code to pass tests. Scope creep → `bd create` a new bead. No Co-Authored-By trailers.

**Step 4 - Race-clean loop (max 10)**
```bash
go test ./... -race -count=3 2>&1
```
Fix root cause, repeat. After 10 failures: return BLOCKED Result Block, release claim, stop.

**Step 5 - Vet & staticcheck**
```bash
go vet ./... && staticcheck ./... 2>/dev/null || true
```
Fix all `go vet` errors. Fix staticcheck except `SA1019` unless unavoidable. Re-run Step 4 if non-test code changed.

**Step 6 - Concurrency & stdlib review (inner sub-agent)**
> Review the following Go diff for: (a) concurrency bugs - TOCTOU races, close-of-closed-channel, nil deref on shared state, goroutine leaks; (b) reinvented stdlib or internal helpers - anything in `sync`, `io`, `os`, `path/filepath`, `slices`, `maps`, or an existing veska internal package, any std lib rewrites (re-invent wheel). (c) CLEAN/SOLID violations, functional options violations. (d) ddd-lite violations. Return numbered findings with file:line. If nothing found, return "LGTM".

Apply every finding. Re-run Step 4 if non-trivial changes result.

**Step 7 - Format & fix**
```bash
go fmt ./... && go fix ./... && go test ./... -race -count=3 2>&1 | tail -5
```

**Step 8 - Commit**
```bash
git add <explicit files>
git commit -m "<type>(<area>): <description>"
```

---

## Result Block

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Issue:    <id> - <title>
Status:   ✓ COMPLETE  |  ✗ BLOCKED (reason)
Clarity:  (no clarification needed) | Asked N questions - description updated
Tests:    +N new  |  N modified
Commit:   <sha> <message>
DoD:
  [x] criterion 1
  [ ] criterion 2 - SKIPPED (reason, new bead: <id>)
Review:   LGTM | <findings addressed>
New beads: <ids or none>

## Context Hand-off
- New types/interfaces: <name in file>
- Files touched: <list>
- Patterns established: <e.g. "golden fixture pattern; see redhat.go">
- Watch-outs: <e.g. "dedup.go assumes OSV feed runs first">
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

For BLOCKED: replace DoD/Review/New beads with `Last failure: <go test -race output, max 20 lines>`. Context Hand-off still required.

---

## Post-Epic Summary (orchestrator)

```
═══════════════════════════════════════════════
Epic $epic_id complete
Closed:  N / M  |  Blocked: K  |  New beads: <ids or none>
═══════════════════════════════════════════════
```

### Feature close-out (if this epic maps to a feature doc)

After closing the epic bead, close out the feature doc:

1. Find the feature doc: `grep -r "beads_id.*$epic_id" docs/features/active/`
2. Update frontmatter: `status: shipped`
3. Move to archive: `mv docs/features/active/FEATURE-xxx.md docs/features/archive/`
4. Regenerate index: `make features-index`
5. Check if the feature's **phase** is now fully shipped:
   - `grep "^phase:" docs/features/active/*.md | sort` - if no active docs remain at that phase, all phase N features are done.

```bash
bd dolt push && git pull --rebase && git push
```