# Persona test suite

Persona-shaped, end-to-end workflows that exercise veska the way a real
**junior dev**, **senior dev**, and **AI agent** would — synthetically, but
against the *real* daemon over MCP + CLI. Where the rest of `tests/mcp` is
organized per-tool, this suite is organized per-**journey**: each test walks a
multi-step workflow and asserts the journey holds, so a regression that breaks
*using* veska (not just a single tool) is caught.

Run it:

```bash
make test-persona          # parity gate + junior/senior/agent workflows (~12s)
make persona-parity        # just the coverage gate (fast, no daemon)
PYTHONPATH=. python3 -m pytest tests/mcp -m persona   # direct
```

No Ollama required — the workflows use the baked-in model2vec embedder. Each
test spawns its own daemon in a throwaway `VESKA_HOME` (the
`test_bootstrap_golden` pattern) and never touches a live install.

## The synthetic corpus

`persona_harness.py` generates one small Go package shaped so the promoted graph
carries, by construction, the facts every workflow needs:

| Symbol | Role |
|---|---|
| `GreetUser` | test-covered fn; **CALLS** `normalizeName` (a real edge) |
| `normalizeName` | callee of `GreetUser` (not dead) |
| `staleHelper` | unexported, uncalled → a **dead-code** finding source |
| `AddNumbers` | exported, no test → an **untested-symbol** finding source |

`persona_workspace(tmp_path)` stands up the daemon, generates + registers the
repo, drains the cold scan, and yields a `PersonaWorkspace` with an MCP client
and CLI/git/sqlite drivers.

## Workflows → SOLO-02 stories

Personas and stories are defined in `docs/design/02-personas-stories`.

### Junior dev — `test_persona_junior.py`

| Phase | SOLO-02 | What |
|---|---|---|
| orient | US-08.01 | `veska repo list` / `veska wiki` to build a mental model |
| find a symbol | US-04.01 | `eng_find_symbol` |
| see findings | US-05.01 | `veska findings list` |
| gate (FAIL) | — | `diff-gate untested` fails on a new untested `Multiply` |
| fix + gate (PASS) | US-03.01 | add `TestMultiply` in the same change → gate passes |

### Senior dev — `test_persona_senior.py`

| Phase | SOLO-02 | What |
|---|---|---|
| structural onboard | US-08.01 | `eng_get_entry_points` / `eng_get_hot_zone` |
| blast radius | US-04.02 | `eng_get_blast_radius` (caller of a changed callee) |
| context pack | US-08.02 | `eng_get_context_pack` (token-bounded neighbourhood) |
| suppression governance | US-05.02 | `eng_suppress_finding` round-trip |
| restart recovery | US-07.01 | promoted state **and** the suppression survive a restart |

### Agent — `test_persona_agent.py`

| Phase | SOLO-02 | What |
|---|---|---|
| ground in context | US-08.02 | `eng_get_context_pack` (symbol mode) |
| walk the call graph | US-02.02 | `eng_get_call_chain` (`GreetUser` → `normalizeName`) |
| staging-aware blast | US-02.02 | `eng_get_dirty_blast_radius`, `included_staging` after an edit |

> **Parked:** SOLO-02's task-anchored Agent stories (US-04.02 / US-09.02 —
> `eng_set_active_task`, task-mode `context_pack`, `eng_get_task_history`) are
> **not reachable on the live MCP surface** (`RegisterTaskTools` is parked off
> the daemon; no MCP path to create a task). Tracked by **solov2-nmps.10**; the
> agent test grows a task-scoping phase when they re-enable.

## Coverage gate — `make persona-parity`

`tools/lint/personaparity` AST-scans the MCP registry and **verifies** every
`eng_*` tool is referenced (as a quoted call) by some `tests/mcp` test — a
persona workflow or the per-tool suite — or is listed with a reason in
`tools/lint/personaparity/parked.txt`. A new tool with no covering test turns
the gate red. This is the "test ALL functionality" guarantee for the MCP
surface; CLI/MCP parity is enforced separately by the `cliparity` lint (the CLI
wraps these same tools).

## Findings surfaced by this suite

Driving the *real* binary turned up real gaps (the point of the suite):

- **solov2-nmps.9** — `diff-gate` dead-code resolution counts the structural
  `CONTAINS` parent edge as a caller, so every dead-code finding reads
  `target_resolved: true` with no fix (false GREEN). The junior gate anchors on
  `diff-gate untested` (sound) until this lands.
- **solov2-nmps.10** — the Agent task-scoping tools are parked off the live
  surface, but SOLO-02 marks their stories "shipped" (doc/coverage mismatch).
