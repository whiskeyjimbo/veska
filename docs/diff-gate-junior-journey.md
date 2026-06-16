# `veska diff-gate` - simulated junior-SWE journey (solov2-ll57.8)

A persona-driven usability run of the **real `veska diff-gate` binary**, distinct
from the ll57.6 e2e tests (which call `Run()` in-process with hand-built fixtures
and assert internal verdict fields). This run approaches the tool the way a
junior engineer would - with only `--help` and the other `veska` commands, no
source - and logs every point of friction.

**Headline:** a junior who has a finding from `veska findings` **cannot** invoke
`diff-gate` for it. The one required input the findings surface never gives them
is the `--anchor` node id. This is a hard blocker, not a polish item.

## The journey

1. **"How do I gate my fix?"** → `veska diff-gate --help`. The flags demand
   `--repo` (a repo id), `--anchor` (a "node id"), and `--rule`. A junior thinks
   in *files and function names*, not repo ids or node ids.
2. **"What's my repo id?"** → guess `veska repo list`. (In a real environment this
   is also where the stale-db migration-tamper wall hits - see F5.)
3. **"What finding am I fixing? What's its anchor/rule?"** → `veska findings list`
   / `findings show`. These print `finding_id`, `rule`, `severity`, `file_path`,
   `message` - **but never `node_id`**. Confirmed in
   `internal/cli/findingscmd/render.go` (`RenderFindingHuman`, the list row, and
   the `--json` `FindingView`).
4. **Dead end.** There is no documented path from a visible finding to the
   `--anchor` node id `diff-gate` requires. (`veska symbol <name>` *does* print
   node ids, but nothing connects a junior from a finding to that command.)

## Friction log

| # | Severity | Finding | Evidence |
|---|----------|---------|----------|
| **F1** | **Blocker** | `--anchor` requires an internal `node_id`; the findings surface never exposes one. No junior path from finding → invocation. | `render.go` has no `node_id`; `--help` says "node id the target finding is anchored on". |
| **F2** | High (robustness) | Against an unindexed/unmigrated repo, the verify path crashes with a raw `no such table: edges` and **emits no JSON verdict** - only `exit 1` + stderr. A CI consumer can't tell "gate FAILed" from "gate errored". | `VESKA_HOME=$(mktemp -d); veska diff-gate … → "diff-gate: evaluate: … no such table: edges", EXIT=1, empty stdout`. Discovery degraded cleanly; verify did not. |
| F3 | Medium | `--repo` wants a repo id the junior must hunt for via `repo list`. | `--help` |
| F4 | Low | `--help` has no example invocation to copy-paste. | `--help` |
| F5 | Env | A stale local db (migration-19 rewrite, ADR-S0017) makes every db-touching command exit 78 with a tamper error - `repo list`, `findings`, and `diff-gate` are all unusable until re-migrated. | `veska repo list → "migration 19 tamper detected"` |

## What worked

- Discovery **degrades to `Ran=false`** on a bad/unindexed db (the fail-safe holds
  in the real binary, not just in `Run()` tests).
- The verdict, *when emitted*, is clean JSON with a stable `failures[]` and
  `new_findings_covered_rules` (the honesty field from ll57.6).

## Recommendations (filed as follow-ups)

- **F1 → accept `--finding <finding_id>`** and derive anchor + rule from the
  stored finding row. This is the junior's natural input (it's the first column
  of `findings list`) and removes `--anchor` *and* `--rule` friction at once.
  *(Alternative/also: `findings show` should print `node_id`.)*
- **F2 → detect "repo not indexed"** (missing tables / unknown repo) and emit a
  clean degraded verdict or a clear actionable error ("repo not indexed - run
  `veska reindex`"), **always emitting JSON**, so the verify path matches the
  discovery path's graceful degradation and CI can parse a verdict.
- **F3/F4 →** add a worked example to `--help` and point `--repo` at the
  `repo list` discovery path.

## Scope note

This complements, not replaces, the ll57.6 e2e logic tests. They prove the gate
*computes* correct verdicts; this run shows a junior *can't reach* that
computation yet. F1 is the gating fix for adoption.

---

## Redo - 2026-06-13 (after ll57.9/.11/.12/.13)

Re-ran the journey against a **freshly built binary** end-to-end on a real repo:
`veska init -y` → `veska repo add --wait` (daemon up) → `findings list` →
`diff-gate --finding`. The first-pass blockers are gone; two **new** bugs
surfaced that only a real-binary, real-daemon run could expose. Both were fixed
and re-validated against the same live binary in this session.

### Fixed since the first run

- **F1 (blocker) → FIXED (ll57.11).** `--finding <id>` is the front door now;
  `--help` documents it and derives `--anchor`/`--rule` from the stored row. A
  junior pastes the `finding_id` from the first column of `findings list` and
  the gate runs.
- **F2 (robustness) → FIXED (ll57.12).** An unindexed/empty repo now emits a
  clean `repo_not_indexed` JSON verdict + non-zero exit (was a raw
  `no such table: edges` crash with empty stdout).
- **F4 → FIXED.** `--help` carries a copy-pasteable example.

### New findings (this run)

| # | Severity | Finding | Status |
|---|----------|---------|--------|
| **N1** | **Blocker** | `diff-gate --repo <short_id>` (the 12-char id printed by `repo add` and accepted by `findings list`) returned `repo_not_indexed` against an **indexed** repo, with a *misdirecting* "index %q first" message. `repoIndexed()` did `WHERE repo_id=?` on the raw flag; `nodes.repo_id` holds the full 64-hex id, and diff-gate - unlike every other surface - never resolved short ids. | **FIXED (ll57.15):** `resolveRepoID` resolves full id / 12-char short id / unambiguous hex prefix → canonical id before any query. Validated: short id now PASSes/FAILs correctly. |
| **N2** | **High (soundness)** | A candidate that **resolved** its target finding but **introduced** a new dead-code finding **PASSed** (`new_findings: null`) - a false GREEN. The discovery clone inherits the indexed graph's `findings` table via `VACUUM INTO`; dead-code/contract-drift are not `AuthoritativeChecker` (only vulnscan is), so the base-side re-check never closes the inherited finding. It leaks into `baseIDs` and, sharing a deterministic id with the candidate's, cancels. Fires only when the index sits **ahead** of base - exactly the local post-commit-hook flow. Clean CI (index == base) is unaffected. The e2e tests missed it because `seedBaseDB` seeds **without** running checks, so their clones start findings-free. | **FIXED (ll57.16):** clear the structural findings in each clone before re-checking, so discovery derives findings purely from graph state. Regression test seeds via a *real* check pass (red without the fix). Validated: the exact command that false-greened now FAILs `new_findings`, exit 1. |

### Lower-friction notes (report-only, not filed)

- **Low-severity dead-code is hidden by default.** `findings list` prints
  `showing 0 of 1 … (1 low-severity hidden; pass --include-low to show)` - the
  hint is there, but a junior's only finding being invisible by default is a
  speed bump.
- **`repo add` before `init` emits a raw sqlite error** (`open sqlite: …
  unable to open database file`) instead of "run `veska init` first" - the
  top-level help does say to init, so this is polish.
- **`repo add --wait` needs the daemon**, but its error is genuinely good: it
  names both `veska service start` and the drop-`--wait` offline path.

### What works (real binary)

- `--finding` front door, clean `repo_not_indexed`, the verdict's
  `new_findings_covered_rules` honesty field, and the discovery fail-safe all
  hold in the live binary, not just in `Run()` tests.
- After the two fixes, the **full junior path is unblocked**: short id in,
  `--finding` in, correct PASS / FAIL[new_findings] / exit code out.
