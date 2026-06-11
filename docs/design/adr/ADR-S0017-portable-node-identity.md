---
id: ADR-S0017
title: "Portable content-derived node identity for a shared multi-contributor graph"
status: Accepted
date: 2026-06-02
deciders: [whiskeyjimbo]
supersedes: []
extends: []
related: [ADR-S0005, ADR-0021, ADR-0020]
verified: false
---

# ADR-S0017 — Portable content-derived node identity for a shared multi-contributor graph

Make a node's `node_id` a function of **what the node is** (repo identity +
repo-relative path + kind + name) rather than **where it was checked out**, so
that two contributors who index the same repository at different absolute paths
produce identical IDs and can share one graph DB without forking every node.

## Status

Accepted. This ADR is the design; implementation is tracked by `solov2-dchd`
and its child beads. Nothing in here has shipped yet.

### Resolved: tier ordering (the lead open question)

The fallback chain is **locked** as:

> **host/path-shaped module manifest** (Go `module github.com/org/repo`; scoped
> npm `@org/pkg`) **> raw canonical `origin` URL > bare module name >
> absolute root.**

A bare (non-host/path) module name (`module myapp`) is demoted **below** the
raw origin URL: it is collision-prone in a shared DB, so it ranks as a
local-stable key only, above abs-root but below any globally-unique anchor.
The bare-name **collision policy** (operator-pinned identity override, or an
extra disambiguating tier) is **deferred** — it is additive (a new tier / an
override path), not a re-key, so it does not gate the core change. Locking this
now is safe without real shared-DB inputs because the migration is drop+rescan,
hence cheap and repeatable while veska is still single-user: the decision is
**reversible, not a one-way door**.

## Context

`treesitter.nodeID` (`internal/infrastructure/treesitter/go.go:392`) computes:

```
node_id = sha256(repoID \x00 path \x00 kind \x00 name)
```

Two of those four components embed the local filesystem layout:

1. **`path` is the ABSOLUTE walked path.** The cold-scan pipeline passes the
   absolute path `filepath.WalkDir` produced (`coldscan.go:236` —
   `stager.stage(ctx, path, rel, d)`; `rel` is computed at `:227` but used only
   for `.veskaignore` matching, not for the ID).
2. **`repoID` itself is `sha256(canonical-abs-root)`**
   (`repo/identity.go:63`). So even with a relative path, the ID would still be
   keyed on the absolute checkout location.

The consequence: the same source, indexed by two people (or by one person on
two machines / CI), yields **completely different node IDs**. The
`solov2-ozoi` epic's Constraints section asserts the opposite — "Node IDs are
content-derived sha256(repoID,path,kind,name) → stable" — which is false as a
portability claim. Today IDs are stable only **per registered repo at a fixed
absolute root** (`solov2-ciie`).

This was acceptable while veska was single-user, single-machine, Go-only. It
stops being acceptable with two roadmap items now in view:

- **Multi-language indexing** (Python + TypeScript). Language-agnostic on its
  own — `ParseFile`/`nodeID` don't care about language — but it widens the
  "what canonically identifies a repo" question across three packaging
  ecosystems (go.mod, package.json, Python packages).
- **A shared graph DB across a few contributors.** This is the decisive change.
  A shared DB only works if identical code resolves to identical IDs regardless
  of who indexed it where. The current scheme structurally cannot do that.

### Identity primitives already exist

The hard part — a stable cross-machine repo identity — is largely already
built, for the URL-cloned-ephemeral-repo feature (`solov2-kxo5.*`):

- `repo.CanonicalURL` (`repo/clone.go`) — normalises any git remote URL form
  (ssh/scp/https/git) to one canonical string.
- `repo.DerivedRepoIDFromURL` (`repo/identity.go:90`) — `sha256(canonicalURL)`.
- `repo.detectOriginURL` (`repo/identity.go:49`) — reads + canonicalises
  `git remote get-url origin`.
- `repo.readModulePath` (`repo/identity.go:145`) — already multi-language:
  reads the go.mod `module` line, falling back to `package.json` `name`.
  `repos.module_path` is already a persisted column.

So this design is mostly about **selecting** the right identity anchor and
threading a relative path — not inventing new machinery.

## Decision

Two coordinated changes, shipped as **one atomic migration** (a half-change is
worse than none — see _Migration_):

### 1. Path component → repo-relative slash path

`nodeID` takes the repo-relative, slash-form path (e.g. `metric/series.go`)
instead of the absolute walked path. Relativisation happens at the **few
entry points** that feed the parser, not at ~70 `ParseFile` call sites:

- **Cold scan** (`coldscan.go`): the staging boundary already has both `path`
  (abs, used to read bytes) and `rel` (slash, currently `.veskaignore`-only).
  Pass `rel` as the parser's path argument. One-line change.
- **Incremental / hot path** (fsnotify watcher) and **git-diff readers**: apply
  the same `filepath.Rel(root, abs)` + `ToSlash` before handing the path to the
  parser.

`nodes.file_path` becomes repo-relative too — this is the correct long-term
shape (a portable DB cannot store absolute paths). The handful of consumers
that need an absolute path on disk (e.g. `savings.go` `os.Stat`) rejoin the
repo root from the registry. The existing `relativizeToRoot` MCP display helper
becomes unnecessary (paths are already relative).

### 2. repoID → a portable identity chain, assigned once and stored

Replace `repoID = sha256(abs-root)` with a **fallback chain**, resolved **once
at `repo.Add` time and persisted** — never re-derived per client:

| Tier | Anchor | Globally unique? | Converges across forks/clones? |
|------|--------|------------------|-------------------------------|
| **a** | canonical git remote URL — `DerivedRepoIDFromURL(CanonicalURL(origin))` | yes (it's a URL) | no — a fork's `origin` points at the fork |
| **b** | in-tree module path — `sha256(readModulePath)` (go.mod `module` / package.json `name`) | no — vanity/`example.com` names collide | **yes — it's committed content** |
| **c** | absolute root (current behaviour) | n/a | **never** — local-only fallback |

The chosen **tier + anchor value** is stored on the `repos` row so the identity
is auditable and deterministic, and so adding an `origin` later does **not**
silently shift a repo from tier c→a and fork every node.

#### Why two anchors, and the Go/TS refinement

Tiers a and b fail on **opposite** axes, which is the whole reason both exist:

- The **remote URL** is globally unique by construction but is *local git
  config* — it diverges across forks and ssh-vs-https remotes (CanonicalURL
  normalises the protocol, not the fork).
- The **module path** is *committed content* — byte-identical across every
  clone and, critically, **inherited unchanged by forks** (a fork of
  `github.com/org/repo` still declares `module github.com/org/repo`). But it is
  not guaranteed globally unique: vanity paths and `example.com/...`
  placeholders collide.

For the actual use case — _a few contributors sharing one DB_ — the in-tree
anchor's fork-stability matters more than the URL's uniqueness, because they
share an upstream, not a checkout location. And by Go convention the module
path **is** the canonical host/path (`github.com/whiskeyjimbo/veska`), so when
it parses as `host/path` it is simultaneously globally unique **and** in-tree —
the strongest possible anchor. The same holds for scoped npm names
(`@org/pkg`). So the refinement:

> When the in-tree module path parses as a canonical `host/path` (or scoped
> package) form, treat it as a **tier-a-equivalent** anchor and prefer it over
> the local `origin` URL — it gives URL-grade uniqueness with content-grade
> convergence. Fall back to the raw `origin` URL only when no module manifest
> exists, and to a bare (collision-prone) module name below that.

**This reorders the chain and is the key decision the implementation bead must
lock down against real shared-DB inputs** (e.g. how to treat a module path that
does *not* look like a host/path — bare `myapp` — without risking a
cross-repo collision in a shared DB). Captured as the lead open question on
`solov2-dchd`.

## The convergence guarantee (the actual contract)

The whole point is one test: _Alice indexes the repo at `/home/alice/src/repo`,
Bob at `/Users/bob/code/repo` → same node IDs → the shared DB dedups._ That
holds **exactly when both resolve the repo to the same anchor**. By the
refinement above, the strongest such anchor is the **in-tree, host/path-shaped
module identity** (Go convention; scoped npm), because it is committed content
*and* globally unique. State each tier honestly:

- **In-tree host/path module identity — converges and is unique.** Committed
  content, so byte-identical across clones and forks; host/path-shaped, so it
  can't collide. The supported shared-DB configuration.
- **Canonical remote URL — converges only when origins agree.** Globally unique
  but local config; a fork's `origin` points at the fork, so forked
  contributors diverge here while their module path still agrees. Used when no
  module manifest exists.
- **Bare module name — non-converging / collision-prone.** Vanity or
  `example.com/...` names can both diverge (renames) and collide (two repos,
  same name). A local stable key, not a sharing guarantee.
- **Absolute root — never converges.** Local-only repos (no remote, no module
  file — e.g. this project today has no git remote, per
  `[[project_no_git_remote]]`) keep working with stable per-root IDs but
  **cannot** participate in a shared DB.

Corollary, and the part most easily gotten wrong: **convergence comes from the
anchor being globally shared, not from each client recomputing identity.**
Identity is assigned once at registration and stored. Two people independently
`repo add`-ing the same upstream must land on the same ID because they share
the committed anchor — not because their local git configs happen to agree on
anything else.

Make non-converging identity **detectable**: `veska doctor` warns when a repo
participating in a shared DB resolved to a bare-module-name or absolute-root
anchor. This is a core part of the feature, not a future nicety — a silent
non-converging repo in a shared DB is the failure mode users will actually hit.

### Known limitations (document, don't solve here)

- **Forks** with different origins get different tier-a IDs and won't dedup
  against upstream. Sharing contributors must agree on a canonical remote.
- **Monorepos**: repo granularity stays "the registered root", unchanged.
  `module_path` is an identity *anchor* for that root, not a re-rooting into
  per-module repos.

## Migration

> **Implementation note (revised down — solov2-dchd):** the shipped migration
> (`0019_identity_scheme_v2.sql`) is a **plain drop+rescan** — it deletes the
> derived graph and clears `last_promoted_sha`, and that is all. The elaborate
> snapshot + repo-id re-key + per-scope **suppression carry-forward** described
> below was built, tested end-to-end, then **deliberately removed** as
> over-engineering for the actual need. The reasoning:
>
> - The id-changing transition is **one-time and single-user** (this ADR
>   sequences it *before* any DB is shared), so the entire downside of a rescan
>   is "re-author a handful of suppressions whose anchor id moved." Cheap.
> - A **shared DB never needs the carry-forward**: node_ids are content-derived
>   and **stable across rescans**, and a rescan never touches the `suppressions`
>   table — so suppressions ride along untouched through any operational
>   downtime+rescan. This ADR forbids node_id from encoding provenance precisely
>   so the shared-DB epic *cannot* force an id re-key; there is therefore no
>   future shared-DB event the carry-forward would protect.
> - Embeddings survive regardless (content-addressed), so the cheap migration
>   keeps the only expensive artifact.
>
> **Revisit only if** a *future* scheme change re-keys ids on an
> *already-shared* DB — at which point design the carry-forward against real
> shared-DB inputs (always the better time, per _Sequencing_). To convert a
> single pre-ADR repo, `repo remove && repo add` re-resolves its identity; no
> migration machinery required. The rest of this section is retained as the
> original analysis of *why* drop+rescan is the right shape.

The migration is the real hazard surface — not the FK rewrite. Two facts,
**both verified against the schema**, shape it:

### Rescan, don't hand-rewrite keys (verified: embeddings survive)

The node/edge graph is a pure function of (source, identity scheme), so the
cleanest migration is **drop nodes/edges + cold-rescan all registered repos**
under the new scheme, rather than an in-place rewrite of every node_id across
7 referencing tables (`nodes` PK, `edges` src/dst FKs, `cross_repo_edge_stubs`,
`node_embedding_refs`, `node_fts`/trigram mirrors, `findings.node_id`).

The expensive artifact — embeddings — **survives the rescan for free**, which
is what makes this cheap:

- `node_embeddings` is PK'd on **`content_hash`**, not `node_id`
  (`0004_node_embeddings.sql`) — content-addressed.
- `nodes.content_hash` (`0001_init.sql:32`, NOT NULL, indexed) is **body-derived
  and invariant** under the identity change.
- Only `node_embedding_refs` is `node_id`-keyed; it is rebuilt on rescan by
  re-pointing each new `node_id` → existing `content_hash` → existing vector.
  The embedder does **not** re-run (dedup-by-content_hash finds the vectors).

This makes rescan the recommended path. (If a future audit shows `content_hash`
is not reliably populated, the calculus flips toward in-place re-key — but the
schema says it is.)

### Carry suppressions forward (verified: all four scopes are affected)

Findings are re-derivable (checks re-run on rescan), so an orphaned
`finding_id` self-heals. **Suppressions are user-authored data and do NOT
self-heal** — and every scope is keyed on something this change moves
(`domain.SuppressionScope`, `suppression.go`):

| Scope | `target` holds | Breaks because |
|-------|----------------|----------------|
| `finding` | `finding_id` = `hash(rule + node_id anchor)` | node_id changes → finding_id changes |
| `symbol` | `node_id` | node_id changes directly |
| `file` | file path | path goes absolute → relative |
| `repo` | `repo_id` | repoID derivation changes |

The migration **must** carry suppressions forward via a deterministic per-scope
key transform (old→new), built during the rescan:

- `symbol` / `finding`: an `old_node_id → new_node_id` map is computable because
  the `(relative-path, kind, name)` tuple is stable across the change — only the
  ID *encoding* of repoID+path moved. Recompute `finding_id` from the new anchor.
- `file`: `filepath.Rel(root, target)` + `ToSlash`.
- `repo`: old `sha256(abs-root)` → new tier-a/b/c id.

Anything that cannot be deterministically remapped (e.g. a tier-c repo whose
abs root changed) is reported, not silently dropped. This is the migration's
acceptance gate. (Relates to `[[ADR-0020]]` suppression conflict resolution and
`[[ADR-0021]]` symbol anchor stability — this ADR changes the anchor *encoding*
while preserving the `(path,kind,name)` anchor *semantics* those ADRs rely on.)

## Consequences

- **Shared DB across contributors becomes possible** for tier-a repos. This ADR
  delivers the *identity scheme and migration* that capability depends on; the
  DB-sharing/sync transport itself is out of scope (its own epic).
- **No regression for local-only repos.** Tier c reproduces today's behaviour
  (stable per fixed root); they simply cannot be shared.
- **`nodes.file_path` is now repo-relative** — a storage-contract change. Disk
  readers rejoin the root from the registry; this is verified-small
  (`savings.go` `os.Stat` is the primary site).
- **One-time rescan on upgrade**, gated behind an identity-scheme version bump
  detected at daemon start. Embeddings are preserved (content-addressed);
  suppressions are migrated; findings re-derive.
- **The coverage manifest re-freezes.** `coverage.NodeKey.ResolveID`
  (`mcp/coverage/manifest.go:85`) currently rejoins `root` to mirror the
  absolute-path hash; post-change it resolves against the relative path + the
  repo's identity tier. The frozen `AlphaRepoID`/`BetaRepoID` constants and the
  manifest self-test update accordingly.
- **`solov2-ozoi` Constraints wording is corrected** to state the true
  invariant (the immediate fix for `solov2-ciie`), independent of when the
  refactor lands.

## Sequencing

**This is independently implementable now — it does not gate on the shared-DB
epic's design.** The relevant ordering constraint is **"before IDs are shared,"
not "after the sharing transport is designed":**

- The anchor analysis is **intrinsic** — the convergence properties
  (in-tree-vs-local-config, unique-vs-collision) are facts about git and the
  module manifests, not about how a DB is synced. The shared-DB transport
  cannot change them.
- The migration is **drop + rescan**, hence cheap **and repeatable**. While
  veska is still single-user with no externally-shared IDs, re-running it costs
  ~nothing and coordinates with no one — so the tier decision is **reversible,
  not a one-way door**. (The earlier draft's "avoid re-keying twice" caution
  contradicted this design's own cheap-migration property and is withdrawn.)
- node_id must **never** encode provenance / namespace / who-wrote-it — that
  would break convergence by construction — so the shared-DB epic cannot force
  a node_id *schema* change beyond the anchor choice. No one-way door exists.

The only parts genuinely better settled with concrete shared-DB inputs — the
bare-module-name collision policy and a possible operator-pinned-identity
override — are **additive** (extra tiers / an override path), not re-keys, so
they don't gate the core change either.

Therefore: land the `ozoi` wording correction immediately (done); implement the
refactor + migration **whenever convenient, the sooner the cheaper**, as one
atomic change that must simply precede any actual DB sharing. The risk is
landing it *late* (after IDs leak into a shared DB), never *early*.
