---
id: SOLO-04
title: "Domain Model — Entities, Aggregates, Invariants"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-07, SOLO-08, SOLO-15]
---

# SOLO-04 — Domain Model

## 1. Purpose

Names the entities, aggregates, value objects, and invariants of
Engram Solo's core domain. Storage schema (SOLO-08), MCP exposure
(SOLO-09), and pipeline orchestration (SOLO-11) all bind to the
terms defined here.

This is one file, not a tree. The domain is small enough that a
sub-tree would obscure rather than help.

## 2. Entity catalog

| Entity | Aggregate role | Identity |
|---|---|---|
| `Node` | Graph-scoped entity, written row-shaped (§11) | `stable_hash(repo_id, language, symbol_path)` |
| `Edge` | Graph-scoped entity, written row-shaped (§11) | `(src NodeID, tgt NodeID, kind EdgeKind)` |
| `Graph` | **Read projection** (§5.3) — *not* an aggregate root | `(repo_id, branch)` |
| `Repo` | **Aggregate root** | `repo_id` (stable hash of root path) |
| `Task` | **Aggregate root** | `task_id` (veska-native ULID) |
| `Finding` | **Aggregate root** | `(finding_id, branch)` where `finding_id = stable_hash(rule, anchor)` is itself branch-stable |
| `Suppression` | Member of `Finding` | `(finding_id, suppressed_at, scope)` |

Seven entities. Three aggregate roots (`Repo`, `Task`,
`Finding`) plus the graph-scoped pair (`Node`, `Edge`) written
row-shaped through `GraphRepository` (§11). `Graph` is a domain
*read projection*, not an aggregate root — codebase graphs at
1M-node scale cannot be loaded whole for every write, so the
write seam is row-shaped and the graph-shaped object is read
only. `Suppression` is a member of `Finding`.

### 2.1 What is not an entity

- **Actor.** There is no `Actor` aggregate. Actors are represented
  by an `actor_id string` (e.g. `"human:jeff"`, `"agent:claude-code"`,
  `"service:veska"`) and an `actor_kind` enum (`'human' | 'agent'
  | 'system'`) on every row that records authorship. That is the
  entire identity model. SOLO-10 has the full story.
- **Workspace.** The daemon has a session and the user has a
  repo. We do not need a third concept that overloads both.
- **Contract.** Endpoint/schema contracts are not modelled as
  domain entities. If they appear later, they enter via a
  parser extension and become `Node`s with `kind = contract`.
- **AuditRun.** The audit log is `audit.jsonl`. It has no
  per-run aggregate.
- **RiskScore.** Risk is computed at query time from `Finding`
  rows; it has no separate identity.
- **CrossRepoStub.** Not an aggregate — a value object owned by
  the source `Node` (§5.2.1). Stored in `cross_repo_edge_stubs`,
  not `edges`, so the `Edge` same-Graph invariant stays absolute.

If any of these come back, it is an ADR. The list above is the
explicit cut.

## 3. Actor representation

Every row written by a state-changing operation carries:

```go
type ActorKind string

const (
    ActorKindHuman  ActorKind = "human"
    ActorKindAgent  ActorKind = "agent"
    ActorKindSystem ActorKind = "system"
)

type ActorStamp struct {
    ActorID   string    // "human:<user>" | "agent:<name>" | "service:veska"
    Kind      ActorKind
}
```

That is the full identity object. There is no composite identity.
There is no `acting_as / on_behalf_of / via` triple.

**The two fields answer different questions and vary
independently.** `Kind` answers "what substrate wrote this row?"
— the listener that accepted the connection (`cli.sock` →
`human`, `mcp.sock` → `agent`) or the daemon goroutine
(`system`). `ActorID` answers "what produced the content?" —
the user, the named agent client, or `service:veska` for
generic daemon work. The pairs that arise in practice:

| `Kind`   | `ActorID` examples | When |
|---|---|---|
| `human`  | `human:jeff`, `human:unknown` | CLI connection on `cli.sock` (SOLO-10 §2.1) |
| `agent`  | `agent:claude-code`, `agent:cursor`, `agent:anon-<rand>` | MCP connection on `mcp.sock` (SOLO-10 §2.2) |
| `system` | `service:veska` | Daemon-internal goroutines: revalidation sweep, auto-link re-scorer, soft-suppression revoker, embed worker (SOLO-10 §2.3) |
| `system` | `agent:<llm-generator-name>` | Review pipeline: a daemon goroutine writes the row, but an LLM produced the rationale; both halves are recorded (SOLO-10 §1.2 review-pipeline exception, SOLO-11 §3) |

The last row is the only `Kind`/`ActorID`-prefix mismatch and
it is intentional: an auditor reading `audit.jsonl` sees both
"the daemon scheduled this write" (`Kind=system`) and "an LLM
produced the content" (`ActorID=agent:<llm>`). The
human-action gate (§3 below, SOLO-10 §3) reads `Kind` only —
`ActorID` is informational, not authoritative.

The `actor_kind` enum is the single human-action-gate input. The one
gate:

```
close.finding[severity >= high] requires actor_kind = 'human'
```

Everything else is open. SOLO-10 is the full doc; this section
exists so SOLO-04 readers know `actor_id` and `actor_kind` are
domain concerns, not identity-subsystem concerns.

## 4. Aggregate boundaries

DDD-lite tactical patterns. Specifically:

- **Aggregate roots have globally addressable write paths.**
  Members of single-entity aggregates (e.g. `Suppression`
  inside `Finding`) write through their root.
- **Cross-aggregate references use IDs, not in-memory pointers.**
  A `Finding` references its anchored `Node` by `NodeID`, never
  by holding a `*Node`.
- **One repository port per write seam.** Four ports total
  (see SOLO-07 §4): `RepoRepository`, `TaskRepository`,
  `FindingRepository` — each carrying the standard
  `Save(ctx, *Entity)` shape (§11) — plus `GraphRepository`,
  which is **graph-scoped** rather than aggregate-rooted: it
  exposes row-shaped writes (`SaveNode`, `SaveEdge`,
  `DeleteFile`) and a graph-shaped read (`LoadGraph`). §11 has
  the full shape and rationale.
- **Each transaction scopes to a single aggregate or graph
  scope.** The promotion transaction (SOLO-08 §5) is the one
  exception: it writes graph rows and a post-promotion queue row together.
  The post-promotion queue row exists so the rest of the work can run
  eventually-consistent.

There are no domain events. There is no second consumer that
would justify them. If one appears, it is an ADR.

## 5. Node, Edge, Graph

### 5.1 `Node`

A symbol in the codebase: function, method, type, file, package.

Required fields:

| Field | Type | Notes |
|---|---|---|
| `id` | `NodeID` | computed by parser |
| `path` | `string` | filesystem path |
| `name` | `string` | display name (last segment of `symbol_path`) |
| `kind` | `NodeKind` | enum |

Optional fields (set via functional options):

| Field | Type |
|---|---|
| `signature` | `*string` |
| `lines` | `*LineRange` |
| `raw_content` | `*string` |
| `content_hash` | `*ContentHash` |
| `language` | `*string` |
| `exported` | `*bool` |

`NodeKind` is closed:

```go
const (
    KindFunction  NodeKind = "function"
    KindMethod    NodeKind = "method"
    KindType      NodeKind = "type"
    KindStruct    NodeKind = "struct"
    KindInterface NodeKind = "interface"
    KindClass     NodeKind = "class"
    KindModule    NodeKind = "module"
    KindPackage   NodeKind = "package"
    KindFile      NodeKind = "file"
    KindField     NodeKind = "field"
    KindTest      NodeKind = "test"
)
```

Eleven kinds. Adding a kind is an ADR. Kinds for documentation,
infrastructure, contracts, etc. are deliberately deferred — they
ride along with the parser that produces them, and we do not have
those parsers.

#### Invariants

1. `id` is stable across renames-within-package; renaming the
   file alone never changes `id`.
2. `content_hash`, when present, equals `sha256(raw_content)`.
   The embedding store layers `model_version` into its own key
   (SOLO-08 §3.3); node identity does not.
3. `language` is non-nil for any code-kind node.
4. `lines.start ≤ lines.end` when present; both are 1-indexed.
5. `exported` reflects source-language visibility — never inferred.

#### `symbol_path` per language

`symbol_path` is the human-readable, language-scoped address of a
symbol. It is part of `node_id`'s hash and is the resolver's key
for cross-repo edges (§5.4). The shipped parsers cover two
languages; the formats are normative.

| Language | `symbol_path` format | Example |
|---|---|---|
| Go | `<import-path>.<TypeName>.<MethodName>` for methods; `<import-path>.<FuncName>` for functions; `<import-path>` for the package node. `<import-path>` is the value of `module_path` from `go.mod` joined with the package directory. | `github.com/org/sdk/auth.Client.Refresh` |
| TypeScript / JS | `<package-or-tsconfig-resolved-module>::<exported-name>(.<member>)*`. The module side is the `tsconfig.paths`-resolved (or `package.json` `name`-relative) module specifier, never a relative path. Non-exported symbols are scoped by their containing exported symbol; truly file-local symbols use `<module>::<file-basename>::<name>`. | `@org/sdk::Client.refresh` |

The format is the parser's responsibility, not the application
layer's. A parser that cannot produce a stable `symbol_path` for
a node does not emit the node. New language parsers extend this
table and ship their format with their grammar.

### 5.2 `Edge`

A directed, kinded relationship between two `Node`s.

Required fields: `src NodeID`, `tgt NodeID`, `kind EdgeKind`.

Optional: `confidence Confidence`, `resolved bool`, `source_line *int`.

`EdgeKind` is closed at five values:

```go
const (
    EdgeCalls     EdgeKind = "CALLS"
    EdgeImports   EdgeKind = "IMPORTS"
    EdgeContains  EdgeKind = "CONTAINS"
    EdgeTests     EdgeKind = "TESTS"
    EdgeDependsOn EdgeKind = "DEPENDS_ON"
)
```

Five kinds. Richer kinds (READS, WRITES, MUTATES, INSTANTIATES,
OVERRIDES, THROWS, CATCHES, DECORATES, IMPLEMENTS, EMBEDS,
EXTENDS, RETURNS_TYPE, TAKES_PARAM, DOCUMENTS, DEPLOYS, …)
require parser support that is not specified; the
blast-radius math is also cleaner with five kinds than with
twenty. A parser producing an edge that does not fit one of the
five drops it on the floor until an ADR adds the kind.

#### Invariants

1. **Same-scope, absolute.** `src` and `tgt` reference `Node`s
   in the same `(repo_id, branch)` scope — i.e. the scope the
   `Graph` read projection (§5.3) is built from. No cross-scope
   references. Both fields are non-NULL. There is no carve-out,
   no exception, no unresolved dst. The invariant is enforced
   at the row level by `GraphRepository.SaveEdge` (§11) and
   structurally by the SOLO-08 §3.1 schema (`dst_node_id NOT
   NULL` plus the composite FK to `nodes(node_id, branch)`).
   Cross-repo bookkeeping lives in a separate value object
   (§5.2.1, `CrossRepoStub`), not in `Edge`.
2. `confidence` is set by the resolver, never by adapters
   directly.
3. `resolved == true` if and only if `confidence != Unresolved`.
4. **Same-repo promotion.** A stored edge requires both endpoints to
   exist in the source graph at promotion time. Same-repo unresolved
   edges are dropped, not stored. Combined with invariant 1,
   every stored `Edge` row has both endpoints in `nodes` —
   structurally, not by application convention.

#### 5.2.1 `CrossRepoStub` (companion value object)

When a parser produces an unresolved import or call whose
intended target lives in another repo (resolver matched by
`module_path` + `symbol_path` but the target repo is not yet
indexed, or hasn't promoted the symbol on its active branch),
the source-side promotion records a `CrossRepoStub` rather than
dropping the reference:

```go
type CrossRepoStub struct {
    SrcNodeID   NodeID    // exists in the source Graph
    Kind        EdgeKind  // CALLS | IMPORTS | DEPENDS_ON
    ModulePath  string    // resolver key
    SymbolPath  string    // resolver key
    Language    string    // shared address space (SOLO-11 §9)
    PromotedAt    time.Time
}
```

A `CrossRepoStub` is **not an `Edge`**. It is a value object
attached to the source `Node` and stored in a sibling table
(`cross_repo_edge_stubs`, SOLO-08 §3.1). It carries no
`Confidence` (it is not a graph edge that could be
"unresolved"; it is a recorded *intent to resolve* whose
output is a synthetic edge at read time). The resolver chain
(SOLO-11 §9) reads stubs alongside `edges` and emits a synthetic
cross-repo edge in the MCP response when the target becomes
resolvable. A stub row is never rewritten in place — promotion
is a read-time projection. Stubs are GC'd when the source
node's containing file is deleted or the source repo is
forgotten.

This keeps invariant 1 absolute: every `Edge` is same-scope,
every `Edge` has both endpoints in `nodes`. Cross-repo
bookkeeping is a separate kind of row with a separate lifecycle.

### 5.3 `Graph`

`Graph` is a **domain read projection** — an in-memory bundle
of `Node` + `Edge` for a given `(repo_id, branch)` scope, built
from rows by `GraphRepository.LoadGraph` (§11). It is **not**
an aggregate root; it has no write methods, and no pipeline
ever calls one. Writes flow row-shaped through
`GraphRepository.SaveNode` / `SaveEdge` / `DeleteFile`.

Operations:

```go
// Read-only traversal helpers used by the resolver chain
// (SOLO-11 §9), blast-radius, and call-chain analysis.
func (g *Graph) Node(id NodeID) (*Node, bool)
func (g *Graph) OutgoingEdges(id NodeID) []*Edge
func (g *Graph) IncomingEdges(id NodeID) []*Edge
func (g *Graph) FindEdges(spec EdgeQuery) []*Edge
func (g *Graph) BlastRadius(start NodeID, depth int) []NodeID
func (g *Graph) CallChain(from, to NodeID) ([]NodeID, bool)
```

Invariants (of the projection, not of any write path):

1. Every `Edge` in the projection has both endpoints in the
   projection. This holds because the underlying rows enforce
   it (§5.2 invariant 1, structurally via SOLO-08 §3.1).
2. `Node` IDs are unique within a `(repo_id, branch)` scope.

#### Why `Graph` is a read projection, not a write seam

A codebase graph at 1M-node scale cannot be loaded whole for
each write. The promotion pipeline (SOLO-11 §2) processes one file
at a time and emits the file's nodes and edges as rows; the
promotion transaction (SOLO-08 §5) is `INSERT INTO nodes (...);
INSERT INTO edges (...);` — there is no `Graph.AddNode` round
trip, and there couldn't be without breaking the promotion budget
(SOLO-13 §3). The honest framing is: **writes are row-shaped,
reads can be graph-shaped.** `Graph` exists where it earns its
keep — the resolver, blast-radius, call-chain — and is absent
from the write paths because it never belonged there.

There is no `Graph.Snapshot()` API. SQLite WAL gives readers a
consistent view for the duration of a transaction; that is the
only snapshot we offer.

#### Where the staging overlay lives

`GraphRepository.LoadGraph` returns the **promoted** state for
`(repo_id, branch)`. The overlay that mixes in `StagingArea`'s
view of dirty files is owned by `GraphReader` in `application/`
(SOLO-07 §4.4a). The MCP router holds a `*GraphReader`, never a
`GraphRepository` directly; reads from the revalidation sweep,
audit-shaped queries, and the cross-repo resolver chain (SOLO-11
§9) call `GraphReader.LoadPromoted` to bypass staging
deliberately.

Cross-file edges under the overlay follow the same drop rule as
same-repo unresolved edges at promotion time (§5.2 invariant 4):
a promoted edge whose target file is marked deleted in staging
is dropped at read time. SOLO-11 §1.2 has the worked example;
this section does not duplicate the rule.

### 5.4 Cross-repo edges (synthetic)

Stored `Edge`s always reference `Node`s in the same `Graph`
(invariant 1, §5.2). Multi-repo working sets — service A indexed
alongside SDK B — produce conceptual edges that cross repos: A's
`CALLS` resolves to a symbol in B, A's `IMPORTS` references B's
package. These cross-repo edges are **not stored**. They are
computed at query time by the resolver chain (SOLO-11 §9) and
returned in MCP responses with `cross_repo: true,
target_repo_id: ..., target_branch: ...`.

Consequences:

1. The same-Graph invariant is preserved. The aggregate boundary
   is unchanged.
2. **Per-query snapshot.** At the start of every traversal that
   may cross repos, the resolver captures a tuple
   `(repo_id, active_branch, last_promoted_sha)` for each
   participating repo (the source repo plus any target repos
   reached by the resolver chain). The traversal reads each
   target as-of its captured `last_promoted_sha`; subsequent
   target-side promotions or checkouts during the traversal do not
   affect the result. The captured tuples are returned in the
   response envelope as `as_of: [{repo_id, branch, promoted_sha}, ...]`
   so the caller can reproduce the read.
3. **Bounded staleness, not point-in-time consistency.** The
   per-target `last_promoted_sha` values are captured at query
   start, not at any single global instant. A 3-repo
   `eng_get_call_chain` may see repo A at a `promoted_sha` that
   landed seconds ago while repo C is still at this morning's
   `promoted_sha`. The result is a *bounded-staleness composition*:
   each target is a self-consistent point in its own history,
   and the envelope declares which point. This is the price of
   not storing a cross-repo edge table; the alternative
   (multi-repo write coordination) is explicitly out of scope.

   **Naming this honestly.** A cross-repo answer is not
   "structural ground truth" — the path it traces may compose
   states that never coexisted at any single moment. Treat
   cross-repo results as **best-effort across the most-recently-
   promoted SHA per repo**. This wording is binding on
   PRODUCT.md, the wiki tour, and any agent-facing tool
   descriptions: nothing about cross-repo traversal should
   imply a single coherent timeline. The single-repo case
   *is* point-in-time consistent and may be described that way.

   Tools that consume the result for correctness-sensitive work
   (review-pipeline LLM passes, blast-radius decisions on a
   `severity >= high` Finding close) MUST surface the `as_of`
   envelope to the human in the close-handoff (SOLO-10 §3.3)
   so the human knows which target states the agent reasoned
   over. The envelope is also returned on every cross-repo
   read (not just close-handoffs) so an agent can detect skew
   itself and flag it instead of silently presenting a composed
   path as authoritative.
4. Cross-repo traversal stops at one hop by default. Tools accept
   `expand_cross_repo: true` to continue. The default keeps
   call-chain and blast-radius queries bounded when the working set
   is large.
5. A cross-repo lookup misses when the target repo is not
   indexed. A `CrossRepoStub` is recorded against the source
   `Node` (§5.2.1) in the `cross_repo_edge_stubs` sibling
   table — **not** in `edges`. The read-time resolver projects
   the stub into the response with `confidence: "unresolved"`,
   `tgt: null`, and `cross_repo_target: {module_path,
   symbol_path, language}` so the agent can see what import
   path failed to resolve. There is no silent drop, and no
   stored `Edge` ever has a NULL endpoint.

This is a deliberate trade against speed: every cross-repo hop is
an indexed point lookup at query time rather than a join over a
materialised table. SOLO-13 §3.4 gates the cost; **OQ-S010** is
the M1 measurement that resolves whether the budget holds. If
OQ-S010 trips, the fallback is a query-time cache invalidated on
target-repo promotion — not an authoritative cross-repo edge table —
and lands as a real ADR against measured numbers, not as
deferred work.

## 6. Repo

The aggregate root for a Git repo the daemon knows about.

`Repo` carries a small bundle of mutable per-repo state
(`active_branch`, `last_promoted_sha`, `module_path`) read by
other aggregates' query paths. The cross-repo resolver
(SOLO-11 §9) projects `Repo` state into traversal results
through the per-query `as_of` snapshot (§5.4 invariant 2): at
traversal start the resolver captures
`(repo_id, active_branch, last_promoted_sha)` for each
participating repo, and reads each target as-of that captured
tuple. This converts the read-side coupling from "depends on
target's *current* state" to "depends on target's state *at a
captured point*"; the write-side scope boundary stays intact
(`GraphRepository.SaveEdge` does not mutate `Repo`).

| Field | Type | Notes |
|---|---|---|
| `id` | `string` | stable hash of root path |
| `root_path` | `string` | absolute filesystem path |
| `added_at` | `time.Time` | |
| `active_branch` | `*string` | last branch observed via `post-checkout` |
| `last_promoted_sha` | `*string` | last commit promoted to promoted state |
| `module_path` | `*string` | e.g. Go module path or npm package name; resolver key for cross-repo edges (§5.4) |

Operations are minimal: register, unregister, rename. The
lifecycle is owned by `veska repo add` / `veska repo remove`
CLI commands.

Branches are not modelled as entities. They are the `branch`
column on `nodes`, `edges`, `findings`, and `post_promotion_queue` rows
(SOLO-08 §4). Switching branches in Git triggers a reparse into
staging; on promotion, rows land with the new `branch` value.

## 7. Task

The aggregate root for a unit of work, optionally pinned to a
tracker issue.

| Field | Type | Notes |
|---|---|---|
| `id` | `string` | veska-native ULID |
| `repo_id` | `RepoID` | task is scoped to one repo |
| `tracker` | `*string` | `"bd"`, `"github"`, `"jira"`, or nil |
| `tracker_ref` | `*string` | e.g. `"bd:veska-42"` |
| `title` | `string` | |
| `active` | `bool` | exactly one task is active per repo |
| `created_at` | `time.Time` | |

The `active` flag is enforced at the SQL layer (a partial unique
index, see SOLO-08 §3.2). Setting a new active task clears the
previous one.

`Task` does not own findings or commits. It is the answer to the
question "what am I working on right now?" — used by the MCP
surface to scope queries, and by the audit log to label state
changes.

## 8. Finding and Suppression

### 8.1 `Finding`

A surfaced concern: a vuln, a dead-code warning, a contract drift,
a structural rule violation.

**Identity is `(finding_id, branch)`**, mirroring `nodes` and
`edges`. `finding_id` is computed as `stable_hash(rule, anchor)`
where `anchor` is `node_id` for symbol-anchored findings or
`file_path` for file-anchored findings — both of which are
themselves branch-stable. The same conceptual finding (same rule
on the same target) shares `finding_id` across branches; only the
per-branch row carries per-branch state (`open`, `closed`).

This shape is required because finding state is genuinely
per-branch:

- A dead-code finding on a symbol deleted on branch A must close
  on A while staying open on main.
- Contract-drift findings are inherently per-branch (signatures
  differ between branches by definition).
- Revalidation (SOLO-11 §6) runs against the active branch and
  may transition the per-branch row without affecting other
  branches.

Cross-branch behaviour for findings that *should* persist across
branches (vulns, secret leaks where the secret is in `.env` on
every branch) is handled by `Suppression` (§8.2), not by
collapsing the PK.

Required fields: `id`, `repo_id`, `branch`, `severity`,
`source_layer`, `rule`, `message`, `state`.

Optional: `node_id`, `file_path`, `closed_at`, `closed_reason`,
`actor_id`, `actor_kind`.

`Severity` enum, **ordered**: `info < low < medium < high < critical`.
Comparisons (`severity >= high`) are well-defined; gates and
filters use the ordering rather than enumerating values.

`SourceLayer` enum, **closed**:

```go
const (
    LayerStructural SourceLayer = "structural"  // tree-sitter / parser-derived
    LayerSemantic   SourceLayer = "semantic"    // embedding / LLM-derived
    LayerSecurity   SourceLayer = "security"    // vuln feeds, secrets scanners
    LayerQuality    SourceLayer = "quality"     // dead code, complexity, drift
)
```

Four layers. The same finding never spans layers; the layer is
who-produced-it, not what-it's-about.

`State` enum: `open | closed`. Two values, not three. The prior
draft carried a `superseded` state distinct from `closed` to
distinguish "the agent's revalidation determined this no longer
applies" from "the human acknowledged and dismissed this." For
solo, the distinction does not drive any agent behaviour and
adds a state machine branch to every consumer. The reason is
captured in `closed_reason: string` — common values include
`"user_dismissed"`, `"revalidated_obsolete"`,
`"human_actiond_ack"` (closing a sticky review-pipeline-failure
finding) — but the field is free-form; new reasons can be
introduced without a state-enum migration.

**No `"superseded_by_revalidation"` closed_reason.** An earlier
draft proposed chaining "old finding → new finding" across a
revalidation when the rule still fires on the new content. That
chain was dropped: `finding_id` is the branch-stable hash of
`(rule, anchor)`, so for a node-anchored finding, re-firing the
rule on new content produces the SAME `finding_id`. There is
nothing to chain. The revalidation sweep instead REFRESHES
`anchor_content_hash` in place on the existing open row — state
stays `open`, `closed_reason` stays NULL, and `actor_id` /
`actor_kind` are left untouched. `"revalidated_obsolete"` is the
only closed_reason the sweep ever writes.

#### Invariants

1. `severity >= high` requires `actor_kind = 'human'` to
   transition `state` to `closed` (the human-action gate).
2. `closed_at` is set if and only if `state = closed`.
3. `closed_reason` is set if and only if `state = closed`.
4. `source_layer` is set at creation and never changes.

### 8.2 `Suppression`

A user/agent decision to silence a finding.

Members of `Finding` (in the aggregate sense) but stored in their
own SQL table for query efficiency.

Required fields: `id`, `scope`, `target`, `reason`, `actor_id`,
`actor_kind`, `created_at`.

`Scope` enum: `finding | symbol | file | repo`.

Optional: `rule`, `expires_at`, `branch`.

**The `branch` field controls cross-branch behaviour.** When
`branch` is NULL the suppression applies on every branch — this
is what you want for vulns, secret leaks, and any rule that fires
against branch-stable inputs (dependency declarations, file paths
in user-managed config). When `branch` is non-NULL the
suppression applies only on that branch — useful for "yes I know
about this dead-code finding on the experimental branch; don't
silence it on main."

The CLI defaults to `branch = NULL` for security-layer findings
and to `branch = <current>` for structural/quality findings. The
agent over MCP must pass `branch` explicitly; there is no implicit
default to avoid agents silently silencing findings on every
branch they didn't intend.

#### Invariants

1. A suppression with `scope = finding` requires the target
   `finding_id` to exist on at least one branch (the suppression
   row references the branch-stable `finding_id`, not a
   per-branch row).
2. `expires_at`, when set, must be in the future at creation
   time.
3. A suppression with `branch` non-NULL requires the branch to
   exist in `repos.active_branch` history at the time of write.
   We do not validate against future branches.

## 9. Value objects

All immutable; equality is structural.

### 9.1 `Confidence`

```go
type Confidence int
const (
    Unresolved Confidence = iota
    Probable
    Strong
    Definite
)
```

Set by the resolver. The default mapping:

- Same-language symbol resolution → `Definite`
- Cross-module / cross-package → `Strong`
- Cross-language (e.g. JSX → JS) → `Strong`
- Contract-based or heuristic → `Probable`
- Cross-repo target not indexed (stub projection, §5.4 inv 5) → `Unresolved`

`Unresolved` is the floor of the enum and is reserved for the
cross-repo-unindexed case: the resolver knows *what* import was
declared but cannot bind it because the target repo is not in
the working set. Same-repo unresolved references are dropped at
promotion (§5.2 invariant 4), not stored as `Unresolved` edges. The
prior name `Speculative` was retired because it implied "low-
confidence guess" — a category the resolver does not currently
emit; heuristic resolution lands as `Probable`. If a future
ADR introduces a genuine "low-confidence guess" case, it gets a
new enum value, not a re-overload of `Unresolved`.

### 9.2 `ContentHash`

```go
type ContentHash [32]byte  // sha256
```

Computed as `sha256(raw_content || ":" || model_version)`. Used
as the dedup key in the embedding store.

### 9.3 `LineRange`

```go
type LineRange struct {
    Start int  // 1-indexed
    End   int
}
```

## 10. Constructor convention

Every entity uses functional options for optional fields:

```go
func NewNode(id NodeID, path, name string, kind NodeKind, opts ...NodeOption) (*Node, error) {
    n := &Node{ID: id, Path: path, Name: name, Kind: kind}
    for _, opt := range opts {
        opt(n)
    }
    if err := n.validate(); err != nil {
        return nil, err
    }
    return n, nil
}

type NodeOption func(*Node)

func WithSignature(sig string) NodeOption    { return func(n *Node) { n.Signature = &sig } }
func WithLines(start, end int) NodeOption    { return func(n *Node) { n.Lines = &LineRange{start, end} } }
func WithRawContent(s string) NodeOption     { return func(n *Node) { n.RawContent = &s } }
func WithContentHash(h ContentHash) NodeOption { return func(n *Node) { n.ContentHash = &h } }
func WithLanguage(lang string) NodeOption    { return func(n *Node) { n.Language = &lang } }
func WithExported(b bool) NodeOption         { return func(n *Node) { n.Exported = &b } }
```

Rules:

1. Required fields are positional.
2. Optional fields go through `With<Field>`.
3. Validation runs once, in the constructor, after options
   apply. Options never validate; they set.
4. No setters. Once constructed, fields change only through
   aggregate-root methods.
5. Outside `core/domain/`, entities are constructed only through
   `New<Entity>`. `layercheck` (SOLO-07 §6) does not catch this
   directly, but the import discipline keeps it tractable —
   `core/domain` is the only package that knows the struct
   internals.

Entities with constructors:

- `NewNode(id, path, name, kind, ...NodeOption)`
- `NewEdge(src, tgt, kind, ...EdgeOption)`
- `NewGraph(repoID, branch, ...GraphOption)`
- `NewRepo(id, rootPath, ...RepoOption)`
- `NewTask(id, title, ...TaskOption)`
- `NewFinding(id, severity, sourceLayer, rule, message, ...FindingOption)`
- `NewSuppression(id, scope, target, reason, actor, ...SuppressionOption)`

## 10a. Naming note: `Actor` and `Kind`

The `Actor` noun in this model collides with the Actor model
(Erlang/Akka). It does not refer to that pattern; an Engram
`Actor` is just an attribution stamp (`actor_id` + `actor_kind`).
The `Kind` suffix on `ActorKind`, `NodeKind`, `EdgeKind`,
`SourceLayer` is consistent project-wide and is unrelated to Go's
`reflect.Kind`. The collisions are noted because they will surface
in code review; this section is the disambiguation.

## 11. Repository port shape

Two shapes, one per kind of write seam.

### 11.1 Aggregate-rooted repositories

`RepoRepository`, `TaskRepository`, `FindingRepository`. Single
entity loads whole, single entity saves whole:

```go
type <Entity>Repository interface {
    Get(ctx context.Context, id <EntityID>) (*<Entity>, error)
    Save(ctx context.Context, e *<Entity>) error
    Find(ctx context.Context, q <Entity>Query) ([]<Entity>ReadModel, error)
}
```

- `Save` covers both create and update. Separate `Create`/`Update`
  invites partial-mutation patterns that violate aggregate
  consistency.
- `Get` returns the whole aggregate.
- `Find` returns read models — projection-shaped types named
  `<Entity>ReadModel`. They do not expose aggregate methods.

### 11.2 Graph-scoped repository

`GraphRepository` is **graph-scoped**, not aggregate-rooted: it
writes `Node` and `Edge` rows individually and reads either
row-shaped projections or the whole `Graph` read object (§5.3).
This is the one departure from the §11.1 shape, and it is
deliberate — see §5.3 "Why `Graph` is a read projection."

```go
type GraphRepository interface {
    // Row-shaped writes. Used by the promotion pipeline (SOLO-11 §2)
    // and the staging overlay (SOLO-11 §1.2).
    SaveNode(ctx context.Context, n *Node) error
    SaveEdge(ctx context.Context, e *Edge) error
    DeleteFile(ctx context.Context, repo RepoID, branch string, path string) error

    // Row-shaped reads for the hot query paths.
    GetNode(ctx context.Context, id NodeID, branch string) (*Node, bool, error)
    FindNodes(ctx context.Context, q NodeQuery) ([]NodeReadModel, error)
    FindEdges(ctx context.Context, q EdgeQuery) ([]EdgeReadModel, error)

    // Graph-shaped read for the resolver, blast-radius, call-chain.
    LoadGraph(ctx context.Context, repo RepoID, branch string) (*Graph, error)
}
```

- `SaveNode` / `SaveEdge` enforce §5.2 invariant 1 at the row
  level: `SaveEdge` rejects rows whose `dst_node_id` is not
  resolvable in the same `(repo_id, branch)` scope. The schema
  (`dst_node_id NOT NULL` + composite FK, SOLO-08 §3.1) is the
  structural backstop.
- `LoadGraph` is the only place a whole-graph object is ever
  materialised. It is read-only; the returned `*Graph`
  exposes traversal helpers (§5.3) and no write methods.
- The promotion transaction (SOLO-08 §5) is the one cross-row
  transactional bundle: many `nodes` rows + many `edges` rows
  + the post-promotion queue row, in one `BEGIN IMMEDIATE`. The
  `GraphRepository` adapter exposes that bundle as a
  per-commit `Promotion(ctx, batch)` call; SOLO-08 §5 has the
  details.

### 11.3 Notes that apply to both shapes

- Cross-aggregate transactions are application-layer
  compositions; the promotion transaction is the one privileged
  case (SOLO-08 §5).
- Read models are projection-shaped (`<Entity>ReadModel`,
  `NodeReadModel`, `EdgeReadModel`); they do not expose
  aggregate methods.

## 12. Diagram

```
Repo ──contains──▶ Graph ──per (repo,branch)──▶ Node, Edge
  │                  ▲
  │                  │ anchors
  │
  ├──per repo──▶ Task
  │
  └──per (repo,branch)──▶ Finding ──contains──▶ Suppression

actor_id + actor_kind stamped on every state-changing row.
```

## 13. Open questions

- **OQ-S007:** Whether `KindTest` warrants a parser pass distinct
  from regular function parsing, or whether tagging functions
  via filename heuristics is sufficient. M2 spike.
- **OQ-S008:** Does the five-edge-kind set hold for the M3
  semantic-search use cases, or do we need IMPLEMENTS / EXTENDS
  for OO languages? Measured against actual queries, not in the
  abstract.

(Canonical definitions live in `design/15-glossary/open-questions.md`.)
