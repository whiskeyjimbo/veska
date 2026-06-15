---
description: "Run the full persona suite, then drive every eng_* MCP tool + its CLI wrapper over the synthetic fixture, capture the REAL outputs, and apply judgment to whether each output matches what it SHOULD be — flagging 'green/valid but wrong, investigate' that deterministic asserts miss. Produces a judgment report (matches / mismatch-investigate / could-not-exercise) and files bd findings for mismatches. Use when asked to verify veska's outputs make sense, sanity-check the whole tool surface, or audit output correctness beyond pass/fail."
arguments: [scope]
allowed-tools: [Bash, Read, Write, Edit]
---

# Persona Verify — output-correctness oracle

`make test-persona` proves the suite is **green**. This skill asks the harder
question a green suite can't: **does the actual output match what it should
be?** A `find_symbol` can return a confidently-wrong ranking; a context pack can
be valid JSON yet miss the obvious neighbour; an error can be graceful yet
misdirecting. You — the model — are the oracle: you read the real outputs and
judge them.

This is the third leg of the persona suite: `test_persona_*.py` (regression),
`/persona-journey` (usability), and this (output correctness).

## Procedure

1. **Green baseline.** `make test-persona`. If red, stop and report — verify
   over a broken suite is meaningless.
2. **Capture the real outputs.** Run the committed driver:
   **`make persona-verify-capture`**. It stands up the synthetic fixture (real
   daemon + Go repo: `GreetUser`→`normalizeName` CALLS edge, covered fn, untested
   `AddNumbers`, dead-code `staleHelper`), enumerates the LIVE surface via
   `tools/list`, and drives every tool with correct params — read tools plus the
   stateful lifecycles (suppress→get→close, close→reopen finding, alias
   set/remove, add/remove a second repo) — printing every request/response
   VERBATIM (`tests/mcp/persona_verify_driver.py`). You KNOW the ground truth of
   this corpus; that is what makes the outputs judgeable.
3. **Mind the coverage line.** The driver asserts every live tool is exercised.
   If it FAILS with `NOT EXERCISED: [...]`, a newly-registered tool has no param
   spec — judge that tool by hand AND add it to the driver's sequence so the
   sweep stays complete. (`scope` may narrow your *judging* focus — e.g.
   `findings`, `blast` — but the driver always sweeps the whole live surface.)
4. **Read the transcript.** Each tool is one `┌─ ✓/✗ <tool>` block with its
   params and verbatim response. A `✗` is a JSON-RPC error — often a deliberate
   strict-param rejection, so judge whether the error is *correct*. Where a tool
   has a `veska` CLI wrapper, the CLI output should agree.
5. **Judge each output** against the known corpus and the tool's contract:
   - **matches** — output is what it should be.
   - **mismatch — investigate (reason)** — green/valid but wrong: a missing
     neighbour, a backwards ranking, a count that contradicts the graph, a
     contract field that lies (cite the expected-vs-actual).
   - **could-not-exercise** — parked, needs state the fixture lacks, or errored
     (note which).
6. **Write the report** to `docs/persona-verify-<date>.md`: a per-tool table
   (`tool | verdict | evidence`), the captured output for every mismatch, and a
   summary line (`N tools: X match, Y mismatch, Z could-not-exercise`).
7. **File bd findings** for each mismatch: `bd create "<tool>: <wrong output>"
   --type bug --description "<expected vs actual, captured output as evidence>"`.
   Link them in the report.
8. **Report** the summary + filed bead ids.

## What counts as a mismatch (judge, don't assert)

- A result set that omits a node you KNOW is in the corpus, or includes one that
  isn't.
- A ranking that puts the wrong hit first (e.g. a fuzzy match above an exact).
- A numeric/structural field that contradicts the graph (edge count, blast
  radius membership, `included_staging` that should be true/false).
- A resolution/verdict that's logically wrong even though the schema is valid
  (cf. solov2-nmps.9: dead-code `target_resolved:true` with no fix — exactly the
  class of bug this skill exists to catch).
- An error message that misdirects (names the wrong cause/fix).

## Rules

- **Ground truth is the synthetic corpus.** Judge against what you KNOW is true
  of it, not against the tool's own self-report.
- **Capture evidence verbatim.** Every mismatch carries the real output.
- **Distrust green.** A passing assert proves no regression, not correctness —
  your job is the gap between them.
- **File, don't fix.** Mismatches become beads; fixes are separate work.
