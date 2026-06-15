# Persona Verify — output-correctness audit (2026-06-15)

Run of `/persona-verify` (solov2-nmps.8) over the synthetic persona fixture
(`tests/mcp/persona_harness.py`). `make test-persona` was green first. Every
non-parked `eng_*` read tool was driven over the fixture and its real output
judged against the **known ground truth** of the corpus:

```
greeter.go:  GreetUser(5-7, exported) → CALLS normalizeName(11-16, unexported)
             staleHelper(20-22, unexported, UNCALLED)
math.go:     AddNumbers(exported, UNTESTED)
greeter_test.go: TestGreetUser → CALLS GreetUser
findings:    dead-code(staleHelper); untested-symbol(staleHelper, normalizeName, AddNumbers)
```

**Summary — 37 non-parked tools: 24 match, 1 mismatch, 12 could-not-exercise.**

## Mismatch (filed)

### `eng_get_dirty_blast_radius` — `included_staging` is always true → **solov2-nmps.11**

With nothing staged (clean checkout), the tool returns
`{"entries": [], "included_staging": true}`. `blastradius.Service.DirtyOf`
hardcodes `resp.IncludedStaging = true` (blastradius.go:486; the `staging==nil`
branch at :460 also returns true). SOLO-09 §4.4 defines `included_staging` as
true *"when staging contributed rows"* — here it contributed none. Misleading
for an agent reading the flag to decide whether unsaved edits are reflected.
Staging itself is correct: after an edit, `entries` did reflect the dirty
`GreetUser` node and its blast (`TestGreetUser`, packages) at t=0. Only the flag
lies. Low severity (contract honesty). Also weakened this suite's own agent test
A3 (it asserted the tautological flag); strengthened to assert the dirty entry.

## Matches (24 read tools — output agrees with ground truth)

| Tool | Verdict | Evidence |
|---|---|---|
| `eng_list_repos` | match | 1 repo, status=promoted, sha set |
| `eng_get_repo` | match | same repo, full id + short id |
| `eng_get_status` | match | ok, repo_count=1, pending_embeds=0, schema=19 |
| `eng_get_config` | match | model2vec(potion-code-16M), vector_backend=memory |
| `eng_find_symbol` | match | GreetUser @5-7, exported, correct signature |
| `eng_get_node` | match | resolves the GreetUser node id |
| `eng_get_file_nodes` | match | greeter.go → {greeter, GreetUser, normalizeName, staleHelper} |
| `eng_find_changed_symbols` | match | HEAD..HEAD → empty (no diff) |
| `eng_list_dependencies` | match | empty (no external deps) |
| `eng_search_semantic` | match | "greeting for a user" → GreetUser ranked #1 (tier top) |
| `eng_search_similar` | match | nearest to GreetUser: normalizeName, TestGreetUser |
| `eng_find_related` | match | greeter.go:4 → GreetUser #1 |
| `eng_find_clones` | match | no clones (corpus has none) |
| `eng_get_blast_radius` | match | GreetUser inbound → TestGreetUser + packages |
| `eng_get_diff_blast_radius` | match | HEAD..HEAD → empty, included_staging=false |
| `eng_get_call_chain` | match | GreetUser→normalizeName CALLS edge, resolved |
| `eng_get_context_pack` | match | focus GreetUser, neighbourhood + token budget |
| `eng_get_entry_points` | match | GreetUser inbound_count=2, has_adjacent_test=true |
| `eng_get_hot_zone` | match | greeter.go top (blast 9), math/test (3) |
| `eng_list_findings` | match | exactly the 4 expected findings, correct anchors |
| `eng_get_finding` | match | dead-code on staleHelper |
| `eng_find_todos` | match | empty (no TODO/FIXME) |
| `eng_find_owner` | match | persona@example.invalid via git_blame |
| `eng_list_suppressions` | match | empty (none suppressed) |

Notable *correct* judgments: `eng_search_semantic` ranked `GreetUser` first for
a natural-language query; `eng_get_entry_points` computed `inbound_count=2`
(test caller + package CONTAINS) exactly; `eng_list_findings` produced the
precise 4-finding set including `untested-symbol` on `normalizeName` (called
only by non-test `GreetUser`).

## Could-not-exercise (12)

- `eng_get_current_repo` — resolves by **cwd**; the harness doesn't `chdir` into
  the repo, so it returns `no indexed repo found for cwd` (correct behaviour for
  the input, just not exercisable in this harness).
- 11 **state-mutating** tools, out of scope for a read-judgment pass (they would
  alter the fixture): `eng_add_repo`, `eng_remove_repo`, `eng_promote_repo`,
  `eng_reindex_repo`, `eng_delete_node`, `eng_close_finding`,
  `eng_reopen_finding`, `eng_suppress_finding`, `eng_close_suppression`,
  `eng_set_repo_alias`, `eng_remove_repo_alias`. (`eng_suppress_finding` is
  separately covered by the senior persona workflow's round-trip.)

## Verdict

The read surface is **sound** over the fixture — 24/25 exercisable read tools
returned outputs that match the known graph, including the ranking- and
count-sensitive ones that asserts rarely check. The single mismatch
(`included_staging` always true on the dirty tool) is the exact "green/valid but
wrong" class this skill targets.

---

# Run 2 — `everything` (mutating lifecycle + fix re-confirmation)

Second `/persona-verify everything` pass, after the Run-1 findings were fixed.
Scope widened to the **state-mutating** tools (exercised as lifecycle
round-trips over the throwaway fixture) plus a re-check of the two fixes.

**Result — no new mismatches.** Both prior findings are confirmed fixed and the
mutating surface behaves correctly.

## Fixes re-confirmed
- **`nmps.11`** — `eng_get_dirty_blast_radius` on a clean tree now returns
  `{entries: 0, included_staging: false}` (was `true`). ✓
- **`nmps.9`** — an unfixed dead-code candidate reads `target_resolved: false`
  (verified live at fix time; unchanged here). ✓

## Mutating lifecycle — all match
| Tool | Verdict | Evidence |
|---|---|---|
| `eng_suppress_finding` | match | returns suppression_id; finding hidden from default `list_findings` |
| `eng_list_suppressions` | match | shows the suppression (scope/target/reason) |
| `eng_get_suppression` | match | returns the row; strictly rejects extra params (`-32602`) |
| `eng_close_suppression` | match | expires the suppression; finding reappears |
| `eng_close_finding` | match | `state: closed` |
| `eng_reopen_finding` | match | reopens (round-trips with close) |
| `eng_promote_repo` | match | already-promoted repo → `files_promoted: 0` (idempotent) |
| `eng_reindex_repo` | match | `status: complete` |

## Notes (report-only, not filed)
- **`eng_set_repo_alias` takes `name`, not `alias`** — strict param validation
  correctly rejected the wrong key (`-32602`, listing allowed params). Good
  contract behaviour, not a bug.
- **`eng_delete_node` is not a real tool.** It appears only as an *example* in a
  `registry.go` comment; it is registered nowhere and no test names it, so a
  call returns `-32601` (correct). The `personaparity` gate correctly excludes
  it from the 40 — confirming the gate counts only genuinely-registered tools.
- **Suppression `actor_id` is `agent:unknown`.** Provenance is recorded
  (`actor_kind: agent`) but the specific agent identity is a placeholder, since
  an MCP session carries no caller identity. Acceptable for the local
  single-user model; revisit only if multi-actor audit is ever needed.

## Verdict (Run 2)
The full surface — read **and** mutating — is sound over the fixture, and the
two defects Run 1 surfaced are fixed. Nothing new to file.
