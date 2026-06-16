# `select-tests` + `doctor identity` - simulated junior-SWE journey (solov2-v6de.2 / dchd.6)

A persona-driven usability run of the **real `veska` binary** for the two
features shipped this session - `veska diff-gate select-tests` (impact-based test
selection, v6de.2) and `veska doctor identity` (identity-tier convergence,
dchd.6). Distinct from the unit/e2e tests (which call the `Run*` helpers
in-process with fixtures): this approaches the tools the way a junior engineer
would - with only `--help` and the other `veska` commands - and logs every point
of friction. Companion to the `diff-gate` run in `diff-gate-junior-journey.md`
(solov2-ll57.8); several findings here are the same shared-`diffgatecmd` weaknesses
re-confirmed on a new subcommand.

**Headline:** `select-tests --help` promises *"NEVER gates: always exits 0.
Emits JSON,"* but **every error path exits 1 with no JSON and raw internal text**
(unknown repo, repo-not-indexed, bad ref range). A junior who wires it into CI on
the strength of that promise - "it always exits 0, I'll just parse the JSON" -
gets an unparseable stderr dump and a non-zero exit instead. This is the same
contract gap `ll57.8 F2` flagged on the verify gates, now on the advisory path
that explicitly advertises the opposite.

## The journey - A: "which tests should I run for my change?"

1. **Obvious first try** → `veska diff-gate select-tests` (no flags). Clean usage
   error, `exit 1`: *"--repo, --base-ref and --candidate-ref are required."* Good.
2. **Read `--help`.** Clear prose + a copy-paste example
   (`--base-ref HEAD~1 --candidate-ref HEAD`). But `--repo` wants a "repo id."
3. **"What's my repo id?"** → `veska repo list`. The `REPO_ID` column
   (`9d998797faf9`) works directly - `resolveRepoID` matches the short id. ✓
4. **Copy the example literally** → `--base-ref HEAD~1 --candidate-ref HEAD` on a
   freshly-branched / shallow / single-commit repo →
   `diff-gate select-tests: list changed files: git diff: unknown revision: refs=HEAD~1..HEAD`,
   `exit 1`, **no JSON**. Raw git plumbing (`refs=HEAD~1..HEAD`) leaks; the
   example's own `HEAD~1` is what fails.
5. **Type the repo's NAME** (it's right there in `repo list`) →
   `--repo taskcli` → *"repo \"taskcli\" not indexed on branch \"main\" - index
   it first"*, `exit 1`. **Misleading:** `taskcli` IS indexed; it's the *name*
   that wasn't resolved. `resolveRepoID` matches exact id / short id / 4-char
   prefix only, then falls through unchanged, so an unknown handle is reported as
   *not-indexed* rather than *unknown repo*.
6. **Eventually succeeds** with the short id + a valid ref range → clean JSON,
   `exit 0`, explicit `"note": "no covering tests selected …"` (the example repos
   have no tests). Honest empty - never silently "all tests." ✓

## The journey - B: "is my graph healthy / shareable?"

1. **`veska doctor`** (the rollup). `identity` is **absent** - by design (the
   shared-DB rollup line was deferred, dchd.6). A junior won't discover it here.
2. **`veska doctor --help`** lists `identity` among the subcommands. ✓
3. **`veska doctor identity`** → `identity: healthy (2 repo(s), 0 non-converging)`
   then `<id> tier=module-hostpath` per repo, `exit 0`. Both text and `--json`. ✓
4. **"What's a tier? Is `module-hostpath` good?"** → `--help` is a single line; no
   legend on the healthy path. The reader sees `0 non-converging` (implies fine)
   but the raw tier name is opaque. The actionable hint only fires when something
   IS non-converging.

## Friction log

| # | Severity | Finding | Evidence |
|---|----------|---------|----------|
| **F1** | **High** | `select-tests` error paths violate the `--help` contract ("always exits 0, emits JSON"). Unknown-repo / not-indexed / bad-ref all `exit 1` with **no JSON** + raw text. CI can't tell "no tests selected" from "tool errored." | no-args/bad-ref/unknown-repo all `exit=1`, empty stdout; only happy+empty path emits JSON at `exit 0`. Mirrors `ll57.8 F2`. |
| **F2** | Medium | `--repo <name>` for a repo *visible in `repo list`* → misleading *"not indexed"*. `resolveRepoID` conflates **unknown handle** with **un-indexed repo**. Shared by every `diff-gate` subcommand. | `--repo taskcli → "repo \"taskcli\" not indexed"`; `resolveRepoID` (diffgatecmd.go:220) returns the input unchanged on no match → `repoIndexed` fails closed. |
| **F3** | Medium | Raw git plumbing leaks on a bad ref range; the `--help` example's `HEAD~1` fails on a shallow/1-commit repo. `git.ErrUnknownRevision` is a **typed** error meant for clean handling, but the command wraps it raw. | `git diff: unknown revision: refs=HEAD~1..HEAD`; `refdiff.go:32` `ErrUnknownRevision`. Shared by all `diff-gate`. |
| F4 | Low | An empty selection can't distinguish "repo has **no indexed tests at all**" (coverage substrate absent) from "your change has no covering tests." | example repos have zero `*_test.go` CALLS edges → always `empty:true`, same note as a genuinely-uncovered change. |
| F5 | Low | No human/plain mode: output is **always** JSON; the ready-to-run `go test` lines sit inside a JSON `commands[]` array. A junior wants to copy-paste a command. (`doctor identity` has both text + `--json` - inconsistent.) | `select-tests` has no `--json` flag because it's JSON-only; `commands[]` must be `jq`'d out. |
| B1 | Low (by design) | `doctor identity` is invisible from the bare `veska doctor` rollup; discoverable only via the subcommand list. | deferred rollup line (dchd.6 - avoids false-warning every solo user). |
| B2 | Low | `doctor identity` healthy-path output + `--help` are jargon-only (`tier=module-hostpath`, `non-converging`) with no legend on what a tier is or why it matters. | `--help` is one line; the explanatory hint only prints when `non_converging > 0`. |

## What worked

- **`doctor identity`** - clean, `exit 0`, text **and** `--json`, and the two repos
  showing identical content-derived ids is a nice live confirmation of ADR-S0017
  portable identity.
- **`select-tests` happy/empty path** - clean indented JSON, `exit 0`, and an
  **explicit `note`** that says *no covering tests* rather than silently
  degrading to "run everything" (AC2 holds in the real binary, not just tests).
- **Short-id resolution** - the `REPO_ID` value printed by `veska repo list` is
  exactly what `--repo` accepts; no hash-hunting beyond the obvious command.
- **Anchored `-run`** - emitted as `^(TestA|TestB)$`, so `TestFoo` won't also drag
  in `TestFoobar`.

## Recommendations (filed as follow-ups)

- **F1 → make the advisory path keep its promise.** On unknown-repo / not-indexed /
  bad-ref, emit a JSON envelope (e.g. `{"empty":true,"note":"…","error":"…"}`) and
  **exit 0**, OR drop the "always exits 0" claim from `--help`. Either way the
  contract and the behaviour must agree, and a CI consumer must always get JSON.
- **F2 → distinguish unknown-repo from not-indexed** in `resolveRepoID`/its
  callers: *"unknown repo 'taskcli' - use the REPO_ID from `veska repo list`."*
  Shared fix across all `diff-gate` subcommands. (Optionally resolve the repo
  *name*/root basename too.)
- **F3 → catch `errors.Is(err, git.ErrUnknownRevision)`** and surface
  *"ref 'HEAD~1' not found - is the base committed / is this a shallow clone?"*
  instead of the raw git string. Shared `diff-gate` fix.
- **F4 → hint when the substrate is absent:** if the repo has zero `*_test.go`
  CALLS edges, say so ("no indexed tests in this repo") so empty isn't ambiguous.
- **F5 → add a plain mode** (or `--commands`) that prints just the `go test` lines.
- **B2 → one-line legend** on the healthy path / in `--help`:
  "tiers: module-hostpath converges (shareable); origin-url/module-bare/abs-root
  are local-only."

## Scope note

This is a usability audit, not a correctness one - the features do what their
tests assert (v6de.2 e2e proves the right test is selected; dchd.6 classifies
tiers correctly). F1–F3 are **delivery-layer** gaps, and F2/F3 are pre-existing
`diffgatecmd` weaknesses that `select-tests` inherits rather than introduces.
