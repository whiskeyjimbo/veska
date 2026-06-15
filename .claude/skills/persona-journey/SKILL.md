---
description: "Role-play a veska persona (junior/senior/agent) through one SOLO-02 lifecycle moment against the REAL binary, using only --help and discoverable surfaces, and log every point of usability friction as a journey doc + bd findings. The judgment counterpart to the automated persona suite: it catches what asserts can't — a junior who can't reach a required input, a misdirecting error, a dead-end. Use when asked to audit veska's usability for a persona, redo a junior/senior journey, or check whether a real person can complete a workflow."
arguments: [persona, moment]
allowed-tools: [Bash, Read, Write, Edit]
---

# Persona Journey — usability audit

Walk **$persona** through SOLO-02 lifecycle moment **$moment** against the real
`veska` binary and daemon, *in character*, logging friction. This is the
**judgment** half of the persona suite (the automated half is
`tests/mcp/test_persona_*.py` + `make test-persona`). Asserts catch regressions;
this catches novel friction — "the finding never gives me the `--anchor` it
demands", "the error says index %q first but the repo IS indexed".

Model: `docs/diff-gate-junior-journey.md` and
`docs/select-tests-identity-junior-journey.md` are prior runs. Match their shape.

## Personas (from docs/design/02-personas-stories)

- **junior** — thinks in *files and function names*, not repo-ids/node-ids. Has
  only `--help` and the other `veska` commands. Pastes ids it sees; does not
  know internals. The harshest usability critic.
- **senior** — comfortable with the model; probes depth (blast radius, context
  pack, suppression governance, multi-step recovery) and CI integration.
- **agent** — an MCP client. Discovers tools via `list_tools`, reasons from
  schemas + descriptions, has no human intuition to paper over a bad contract.

## Lifecycle moments

`01 first install · 02 daily edit · 03 commit/push · 04 query · 05 findings ·
06 branch switch · 07 daemon restart · 08 onboarding · 09 session recovery`

## Procedure

1. **Build fresh.** `make build`. Stand up a throwaway install — reuse the
   harness (`tests/mcp/persona_harness.py::persona_workspace`) for a real
   daemon + synthetic repo, OR `veska init -y` + `veska repo add` in a tmp
   `VESKA_HOME` for a true zero-state run. Never touch the live `~/.veska`.
2. **Stay in character.** Start from the persona's natural entry point
   (`veska --help`, a finding id from `findings list`, an editor MCP call).
   Use ONLY what that persona would discover. When you reach for internal
   knowledge to get unstuck, that reach *is* the finding — log it.
3. **Log every friction point** as you go: severity (Blocker / High / Medium /
   Low / Env), the friction, and **evidence** (the exact command + output, or
   the `--help` line / `render.go` field that's missing).
4. **Note what worked** too — graceful degradation, honest errors, good
   pointers — so the doc is balanced.
5. **Write the journey doc** to `docs/persona-journey-<persona>-<moment>.md`
   with: a one-line headline (the biggest blocker), the journey narrative, a
   friction table (`# | Severity | Finding | Evidence`), a "what worked"
   section, and recommendations.
6. **File bd findings** for each Blocker/High (and Medium worth fixing):
   `bd create "<friction>" --type bug --labels area:<surface>,kind:bugfix
   --description "<persona/moment, evidence, recommended fix>"`. Link them in
   the doc. Lower-severity notes can stay report-only (say so).
7. **Report** the headline + the filed bead ids. Surface nothing intermediate
   unless blocked.

## Rules

- **Real binary only.** No in-process `Run()` calls, no mocked fixtures — the
  value is what a real invocation does, including crashes and raw errors.
- **Evidence or it didn't happen.** Every friction row cites a command+output
  or a concrete code/`--help` location.
- **Don't fix in this skill.** This audits and files; fixes are separate beads
  (keeps the journey honest and the diff reviewable).
- **One persona × one moment per run.** Scope creep dilutes the findings.
