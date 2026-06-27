# Veska diff-gate Action (v0)

Merge-time, graph-aware diff gates for a pull request. Fails the check when a
change introduces a regression the normal merge bar (compile + tests) misses
*because it compiles green*:

| gate | catches |
|---|---|
| `api` | breaking change to an exported symbol's signature shape |
| `security` | net-new hardcoded secret / vulnerable dependency |
| `cycles` | net-new dependency cycle |
| `clones` | net-new exact-duplicate code |
| `untested` | a changed prod symbol no test reaches |

Validated by benchmark (epic `tzq9`): `api` + `security` are clean wins (zero
false positives on benign diffs); `untested` is clean once the branch is
resolved; `clones` is exact-only (route fuzzy duplicates to `veska duplicates
--near`, which is advisory, not a gate).

## Status: v0 - NOT yet validated in a live CI run

The command sequence is grounded in the real CLI, but `action.yml` has not run on
a real GitHub runner yet. **Do not add a live triggering workflow until the
validation pass below is green** (an untested workflow would block real PRs).

## Usage (after validation)

```yaml
# .github/workflows/diff-gate.yml
name: diff-gate
on:
  pull_request:
permissions:
  contents: read
  security-events: write   # for the SARIF upload (GitHub Security tab)
  pull-requests: write     # for the sticky PR comment (gate verdicts + advisory report)
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 1            # the action fetches the two endpoint SHAs itself
      - uses: ./.github/actions/diff-gate
        # with:
        #   gates: "api security cycles clones untested"
```

## How it works

1. **Fetch** the PR base + head SHAs (targeted two-SHA fetch, not full history -
   a two-dot `base..head` diff only needs the two trees).
2. **Build** veska (`make build-small` - thin cgo build; no usearch).
3. **Index the base** state via `veska reindex` (standalone direct-sqlite
   cold-scan; no daemon). The gate's indexed-HEAD graph == the base; the
   candidate diff is read from git at gate time.
4. **Run each gate** with `--base-ref base.sha --candidate-ref head.sha`. The
   branch auto-resolves to the active (indexed) branch - no `--branch` needed
   (fixed in `i0tx.7.1`; before that, omitting `--branch` silently defaulted to
   `main` and misled).
5. **Fail** the check if any blocking gate fails; `failed-gates` output lists them.

## v0 limitations (tracked follow-ups)

- **Indexes from scratch every run** - no base-graph cache (`i0tx.3.1`) or
  no-embed gate-ready fast path (`i0tx.10`) yet. Fine for small/medium repos;
  slow on large ones until those land.
- **Go-only** today.

## PR comment

On a `pull_request` event the action posts ONE sticky comment (found + updated
by a hidden `<!-- veska-diff-gate -->` marker, so re-runs edit in place rather
than pile up): a ✅/❌ verdict row per gate plus the advisory `report`
(blast radius, change-risk, open findings, changed-but-untested). The report is
rendered with `veska diff-gate report --format markdown` and never gates - it
always exits 0. Needs `pull-requests: write` (set above); the step is
best-effort, so a comment failure never masks the gate's pass/fail exit. The
per-symbol detail lives in the SARIF alerts (Security tab); the comment is a
verdict summary + the advisory body.

## SARIF (GitHub Security tab)

Each gate also emits SARIF 2.1.0 (`--format sarif`); the action writes one file
per gate into a temp dir and uploads them in a single `upload-sarif` submission,
so gate findings show as code-scanning alerts. Notes:

- Needs `security-events: write` (set above). The upload is best-effort
  (`continue-on-error`) so a missing code-scanning entitlement never masks the
  gate's own pass/fail exit.
- Locations are line-level for `api` / `clones` / `cycles` / `untested` (a newly
  added symbol the base index can't resolve falls back to a file-level anchor).
  `security` findings are file-level: a secret anchors to its file and a
  vulnerable dependency to its manifest; the matched line survives in the alert
  message text. (True secret line-level is a tracked follow-up.)
- A PASS gate emits an empty-results SARIF that still declares its rules, so a
  fixed alert auto-clears on the next run.

## Validation pass (the next step before shipping)

Run the action end-to-end on a real GitHub runner against a throwaway PR that
contains one known regressive diff (e.g. a breaking api change) and confirm:
(a) it builds + indexes, (b) the gate fails the check with the right gate named,
(c) a benign PR passes. Capture the run, then wire the live workflow above.
