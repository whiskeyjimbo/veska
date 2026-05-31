---
id: SOLO-07
title: "Architecture — Layering, Ports, Composition Root"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-03, SOLO-04, SOLO-08]
verified: true
verified_date: "2026-05-17"
---

# SOLO-07 — Architecture

## 1. Purpose

The code organisation: hexagonal layering, the package layout, the
ports map, the composition root, and the one lint check that keeps
the layers honest. One impl per port; a second impl is a future
ADR, not a present design constraint.

## 2. The layers

```
                ┌─────────────────────┐
                │       cmd/          │   binary entry points
                └──────────┬──────────┘
                           │
                ┌──────────▼──────────┐
                │ cmd/veska-daemon/   │   composition root (manual DI;
                │   wire.go           │   wire.go lives in the binary pkg)
                └──────────┬──────────┘
                           │
            ┌──────────────┴──────────────┐
            │                             │
  ┌─────────▼─────────┐         ┌────────▼────────┐
  │  application/     │         │ infrastructure/ │
  │  (use cases)      │         │  (adapters)     │
  └─────────┬─────────┘         └────────┬────────┘
            │                             │
            └─────────────┬───────────────┘
                          │
                ┌─────────▼─────────┐
                │   core/ports/     │   Go interfaces
                └─────────┬─────────┘
                          │
                ┌─────────▼─────────┐
                │   core/domain/    │   pure entities
                └───────────────────┘
```

**Import direction.** Read the diagram bottom-to-top: each layer
may import only from layers *below* it on the page, not above.
`core/domain` imports nothing. `core/ports` imports only
`core/domain`. `application` and `infrastructure` both import
`core/domain` and `core/ports` — they are sibling layers and
must not import each other. The composition root is the only
place that imports both `application/` and `infrastructure/`
(so it can wire concrete adapters into use cases). The
composition root for the daemon ships *as part of the binary
package* — `cmd/veska-daemon/wire.go` — rather than in a
separate `internal/bootstrap/` package. `internal/bootstrap/`
exists only as a `doc.go` placeholder; the actual wiring lives
next to `main.go` in each `cmd/` sub-package. `cmd/` is
therefore the layer that imports both `application/` and
`infrastructure/`.

This makes "imports flow downward" precise: in the diagram,
arrows point at what a layer *depends on*. The `application` ↔
`infrastructure` arrow does **not** exist, even though both
boxes sit at the same level — the dependency between them flows
through `core/ports`.

### 2.1 Per-layer responsibilities

- **`core/domain/`** — entities, value objects, aggregate roots
  (SOLO-04). Pure functions; no I/O; standard library only.
  Constructors are functional options.
- **`core/ports/`** — Go interface definitions. The full set
  of storage, substrate, and service ports listed in §4. Files
  are named for the capability they expose (`graph.go`,
  `queue.go`, `embedder.go`, `vector.go`, `edge_reader.go`,
  `node_lookup.go`, `finding_storage.go`, …) — there is no
  `*_repository.go` naming scheme. No implementations.
- **`application/`** — use-case orchestrators. The `Ingester`
  (the promotion hot path). The post-promotion-queue-drain goroutines (one per
  `work_kind`). The MCP request router. Talks to ports only;
  never to SQLite or HTTP directly.
- **`infrastructure/`** — port implementations. SQLite repos,
  the tree-sitter parser, Ollama HTTP clients, the MCP transport,
  fsnotify, slog logger. Never imports `application/`.
- **`cmd/`** — one sub-package per binary (`veska`,
  `veska-daemon`, `veska-mcp`). Flag parsing, signal handling,
  and — for the daemon — the composition root itself. The
  daemon's wiring lives in `cmd/veska-daemon/wire.go` (with
  `providers.go` alongside it); manual constructor wiring, no
  DI container. Reading `cmd/veska-daemon/wire.go` shows you the
  entire daemon wiring.
- **`internal/bootstrap/`** — a placeholder package containing
  only `doc.go`. No wiring shipped here; the composition root
  moved into the `cmd/` binary packages. Other cross-cutting
  internal packages (`config`, `observability`, `repo`,
  `backup`, `doctor`, `service`, …) are listed in §3.

## 3. Package layout

This is the shipped layout. Where a path below differs from an
earlier draft, the code is canonical (per CLAUDE.md).

```
veska-v2/
  cmd/
    veska/                     # CLI binary
    veska-daemon/              # daemon binary; wire.go is the composition root
      main.go
      wire.go                  # newDaemon — manual DI wiring
      providers.go             # per-dependency constructors
    veska-mcp/                 # stdio shim binary
  internal/
    core/
      domain/                  # pure entities: Node, Edge, Graph, Task,
                               #   Finding, Suppression, Embedding, ParseResult,
                               #   Actor helpers
      ports/                   # ~24 Go interface files, named for the
                               #   capability (NOT *_repository.go):
                               #   graph.go, edge_storage.go, edge_reader.go,
                               #   node_lookup.go, queue.go, vector.go,
                               #   embedder.go, embedding.go, embedding_refs.go,
                               #   parser.go, lexical.go, watcher.go,
                               #   notifier.go, tracker.go, llmgenerator.go,
                               #   vulnsource.go, audit.go, revalidate.go,
                               #   resolved_edge.go, finding_storage.go,
                               #   finding_querier.go, todo_querier.go,
                               #   contract_drift.go, deadcode.go
    application/               # use-case orchestrators
      ingester.go              #   promotion hot path
      promoter.go              #   atomic promotion orchestrator
      promotion_store.go       #   PromotionStore port (defined here)
      staging.go               #   in-memory StagingArea
      ingestion_gate.go
      resync.go
      autolink/                #   per-feature sub-packages, each with a
      blastradius/             #   handler.go + core logic + tests
      checks/                  #   checks + contractdrift + deadcode
      contextpack/
      embedder/
      revalidate/
      search/
      wiki/                    #   hot_zone, entry_points, render
    infrastructure/            # port implementations (adapters)
      sqlite/                  #   graph + FTS storage; PromotionStore + sinks
        migrations/
        queue/                 #   post-promotion queue store
        resolver/
      vector/                  #   dual-backend VectorStorage
                               #   (sqlite-vec default; usearch HNSW under
                               #   the hnsw_native build tag)
      embedding/
        ollama/                #   Ollama EmbeddingProvider adapter
      treesitter/              #   CodeParser (Go, TS/TSX/JS/JSX)
      git/                     #   fsnotify watcher, diff/history readers, hooks
      mcp/                     #   MCP server, tool registry, overlay
      llm/                     #   Ollama LLMGenerator adapter
      audit/                   #   audit-log writer + redaction
      beads/                   #   bd-cli Tracker adapter
      fs/                      #   .veskaignore handling
      notifier/                #   stderr Notifier
      vulnsource/              #   VulnSource (null impl today)
    backup/                    # backup create/verify + vector round-trip
    config/                    # paths.go — ~/.veska resolution
    crashloop/                 # crash-loop breaker
    doctor/                    # veska doctor diagnostics
    embedderprobe/             # Ollama embedder probe
    logrotate/                 # daemon-log rotation
    observability/             # metrics, spans, embedder instrumentation
    repo/                      # repos-table registry + active-branch watch
    service/                   # systemd / launchd service management
    tokenize/                  # symbol tokenisation helper
    bootstrap/                 # placeholder package — doc.go only
  tools/
    loadtest/                  # eval / load-test harnesses (build tag `eval`)
  docs/
  go.mod
  Makefile
```

The `layercheck` tool (the one mandatory architectural analyser)
lives in its own `tools/` location; see §6.

A few things this layout deliberately does not have:

- **No `[L]` / `[W]` / `[C]` mode branches.** One mode. The
  daemon's `newDaemon` wiring takes no mode argument.
- **No `ErrCapabilityDeferred`.** A capability is either
  implemented or removed. There are no port methods that compile
  and return "not yet."
- **No replication / webhook / canonical packages.** Those are
  parked in `deferred/`.
- **No worker-pool taxonomy.** The post-promotion queue has one goroutine per
  `work_kind`. That is the entire worker model.
- **No per-impl factory directories *until* a second impl ships.**
  Each port has one impl at M1; provider-keyed selection
  (`[<port>].provider = "x" | "y"`; SOLO-05 §1.1) becomes
  legitimate the moment a second impl lands behind an ADR.
  CONFIG-SURFACE.md already lists four ports with `provider`
  keys (Embedder, LLMGenerator, Tracker, VulnSource); some of
  those second impls land in M3+ as the matching ADR ratifies
  them.

## 4. Ports map

`core/ports/` ships roughly two dozen Go interface files.
Hexagonal architecture distinguishes two directions:

- **Driven (outbound) ports** — the application *calls out*
  through them; infrastructure adapters *implement* them
  (repos, embedder, file watcher, …).
- **Driving (inbound) ports** — infrastructure adapters *call
  in* through them; the application *implements* them
  (the MCP transport adapter calls `RPCHandler`, which the
  application's request router implements).

Both categories live in `core/ports/`. The import direction
from §2 stays absolute: `infrastructure/` imports
`core/ports/` (and `core/domain/`), never `application/`.
A driving adapter holds a port interface, not an application
struct.

SOLO-07 is the canonical catalogue (this section); SOLO-05
covers the eleven that are plugin-swappable: Embedder,
LLMGenerator, Tracker, VulnSource, SecretsScanner, Notifier,
CoverageSource, OwnershipSource, FileWatcher, CodeParser,
TokenEstimator. The other nine (4 repository ports + 2
storage adjuncts + Logger + RPCHandler) are
not plugin candidates — a second `GraphRepository` would mean
changing the substrate, not swapping an adapter; Logger is a
substrate primitive whose port exists for testability rather
than runtime swap; the two driving ports each have one
in-process implementation because there is one of each surface.

> **Note on naming.** The tables in §4.1–§4.3 below describe the
> *roles* the ports play. The actual files in `core/ports/` are
> named for the capability they expose — `graph.go`,
> `edge_storage.go`, `finding_storage.go`, `queue.go`,
> `vector.go`, `embedding.go`, `embedder.go`, `parser.go`,
> `watcher.go`, etc. — and the adapter files under
> `infrastructure/sqlite/` follow the same convention
> (`edge_repo.go`, `edge_reader_repo.go`, …). There is no
> `<Entity>_repository.go` file-naming scheme on disk; treat the
> "Impl" column paths below as illustrative of which adapter
> package owns the implementation, not as literal file names.

### 4.1 Storage ports

Aggregate-rooted storage plus one graph-scoped reader/writer
(SOLO-04 §11):

| Port | Scope | Shape | Impl |
|---|---|---|---|
| `RepoRepository` | aggregate root `Repo` | `Save(ctx, *Entity)` (§11.1) | `infrastructure/sqlite/repo_repository.go` |
| `TaskRepository` | aggregate root `Task` | `Save(ctx, *Entity)` (§11.1) | `infrastructure/sqlite/task_repository.go` |
| `FindingRepository` | aggregate root `Finding` | `Save(ctx, *Entity)` (§11.1) | `infrastructure/sqlite/finding_repository.go` |
| `GraphRepository` | graph scope `(repo_id, branch)` | row-shaped writes + graph-shaped read (§11.2) | `infrastructure/sqlite/graph_repository.go` |

The first three follow the standard shape:

```go
type <Entity>Repository interface {
    Get(ctx context.Context, id <EntityID>) (*<Entity>, error)
    Save(ctx context.Context, e *<Entity>) error
    Find(ctx context.Context, q <Entity>Query) ([]<Entity>ReadModel, error)
}
```

`Save` does not take a separate identity parameter. The aggregate
itself carries `actor_id` and `actor_kind` on the rows being
written; SOLO-04 §3 explains why that is enough.

`GraphRepository`'s row-shaped methods (`SaveNode`, `SaveEdge`,
`DeleteFile`) follow the same identity-on-the-row rule. Its
graph-shaped read (`LoadGraph`) returns a `*Graph` (SOLO-04
§5.3) — a domain read projection with traversal helpers, no
write methods. SOLO-04 §11.2 has the full surface and
rationale.

### 4.2 Storage adjuncts

| Port | Purpose | Impl |
|---|---|---|
| `EmbeddingStore` | Read/write content-addressed embedding bytes | `infrastructure/sqlite/embedding_store.go` |
| `VectorIndex` | ANN search over sqlite-vec | `infrastructure/sqlite/vector_index.go` |

These are not full aggregates; they are key/value-shaped adjuncts
to `GraphRepository`. Splitting them out keeps the embedding
worker and the search-time path narrow.

### 4.3 Substrate

| Port | Purpose | Impl | First needed |
|---|---|---|---|
| `CodeParser` | Source → nodes/edges | `infrastructure/treesitter/` | M1 |
| `Embedder` | Text → vector | `infrastructure/ollama/embedder.go` | M1 |
| `LLMGenerator` | Prompt → completion | `infrastructure/ollama/generator.go` | M5 (review) |
| `Tracker` | Read tracker issues | `infrastructure/trackers/bd/` | M2 |
| `FileWatcher` | fsnotify abstraction | `infrastructure/fs/watcher.go` | M1 |
| `Logger` | Structured logging | `infrastructure/logger/slog/` | M1 |
| `SecretsScanner` | Scan diff hunks for secret-shaped strings | `infrastructure/secrets/builtin/` | M2 |
| `OwnershipSource` | Resolve owners for a file or symbol | `infrastructure/ownership/codeowners/` | M2 |
| `Notifier` | Push finding-arrived events | `infrastructure/notifier/stderr/` | M2 |
| `VulnSource` | Return advisories for a dependency set | `infrastructure/vuln/osv/` | M3 |
| `CoverageSource` | Ingest coverage reports | none | future |
| `TokenEstimator` | Estimate response token count | `infrastructure/tokens/charsdiv4/` | M5 (review caps) |

Twelve substrate ports + four repository ports + two storage
adjuncts + one driving port (§4.3a) = nineteen. `CoverageSource`
ships without an impl; the rest have exactly one. Of the twelve
substrate ports, eleven are plugin-swappable per SOLO-05;
Logger is the substrate primitive (one impl, slog-backed;
testable via a recording fake).

### 4.3a Driving (inbound) port

| Port | Purpose | Caller (adapter) | Implementer (application) | First needed |
|---|---|---|---|---|
| `RPCHandler` | Dispatch a single JSON-RPC frame to the right router by verb namespace (`eng_*` MCP tools or daemon-control verbs) | `infrastructure/mcp/uds/` | `application/rpc_router.go` (composes `MCPRouter` + `ControlRouter` internally) | M1 |

**One driving port, two internal routers, one wire.** Both
`cli.sock` and `mcp.sock` carry JSON-RPC 2.0 frames. The UDS
adapter calls `RPCHandler.Handle(ctx, frame)`; the application's
top-level router dispatches to its `MCPRouter` (the
`eng_<verb>_<object>` tools, SOLO-09 §3) or `ControlRouter` (the
daemon-lifecycle verbs `Promote`, `BackupCreate`, `EmbedderSwap`,
`DoctorRun`, etc.). The split between routers is *internal to
the application layer* — the prior design exposed two driving
ports for what is one inbound seam, and that surface is now
consolidated.

A test that exercises a router without a live socket constructs
the top-level `RPCRouter` directly and calls `Handle`; a test
that exercises the UDS frame parser hands the adapter a fake
`RPCHandler` and asserts on the dispatch.

If a second MCP transport lands (gRPC, named pipe), it
implements no port — it is another *driving adapter* that
holds the same `RPCHandler`. There is no `MCPTransport`
abstraction; transport variation lives in adapter code.

### 4.4 What is not a port

- **Configuration loading** — plain Go in `config/`. Tests pass a
  struct.
- **Path resolution** (`~/.veska`) — plain Go in `config/paths.go`.
- **Git operations beyond reading** — Veska never writes Git.
- **MCP transport** — there is one (Unix socket); no abstraction
  needed. Adding a second would be an ADR and would introduce a
  driving adapter, not a port (the port is `RPCHandler`, §4.3a).
- **`StagingArea`** — plain Go interface in `application/staging.go`.
  Both the save pipeline (writer) and the MCP router (reader)
  are application code, so this is an intra-application
  testability seam — *not* a hex seam, no direction crossed.
  Single in-memory impl.
- **`PostPromotionQueueDrainer`** — plain Go interface in
  `application/post_promotion_queue_drain.go`. Bridges the promotion pipeline
  (writer) and per-`work_kind` drain goroutines (reader); both
  application code. Same status as `StagingArea` — intra-layer
  testability seam, not a port.

### 4.4a `GraphReader` — the staging-overlay reader

The MCP graph-read tools (`eng_get_node`, `eng_get_call_chain`,
`eng_get_blast_radius`, `eng_find_symbol`, etc.) need a view that
combines `StagingArea` (in-memory, dirty files) with
`GraphRepository` (promoted SQLite rows) per the SOLO-11 §1.2
per-file overlay rule. That composition is **named, owned, and
located in `application/`** — it is not implicit in the MCP
router and it is not a method on either underlying primitive.

```go
// application/graph_reader.go
type GraphReader struct {
    repo    ports.GraphRepository
    staging *StagingArea
}

func (r *GraphReader) Node(ctx context.Context, id NodeID, branch string) (*Node, bool, error)
func (r *GraphReader) LoadGraph(ctx context.Context, repoID RepoID, branch string) (*Graph, error)
func (r *GraphReader) FindNodes(ctx context.Context, q NodeQuery) ([]NodeReadModel, error)
func (r *GraphReader) FindEdges(ctx context.Context, q EdgeQuery) ([]EdgeReadModel, error)
```

`GraphReader` is the **only** place the per-file overlay rule is
implemented. Properties:

1. **One owner of the merge rule.** SOLO-11 §1.2 is the spec;
   `GraphReader` is the impl. The MCP router holds a
   `*GraphReader`, never a `GraphRepository` directly. Tools that
   intentionally read promoted-only state (audit-shaped queries,
   the revalidation sweep) call `GraphReader.LoadPromoted(...)` —
   a documented sibling that bypasses staging. The plain
   `LoadGraph` always overlays.
2. **Cross-file edge resolution under overlay.** When a traversal
   crosses into a file with a staging entry, the target's rows
   come from staging; if staging marked the file deleted, the
   edge is **dropped** at read time, mirroring the same-repo
   unresolved-edge drop at promotion time (SOLO-04 §5.2 invariant
   4). No new edge state, no new degraded reason — the edge
   simply does not appear in the response. SOLO-11 §1.2 has the
   worked example.
3. **Branch scoping.** Staging is implicitly scoped to the repo's
   `active_branch`. Reads against any other branch bypass
   staging; the response sets `included_staging: false`.
4. **Not a port.** Both `StagingArea` (writer: save pipeline;
   reader: GraphReader) and `GraphRepository` (reader/writer:
   `application/`) sit in or below the application layer. No hex
   seam crosses; the abstraction exists for testability and to
   give the merge rule a single home.
5. **Cross-repo.** The cross-repo resolver chain (SOLO-11 §9)
   composes per-repo `GraphReader.LoadPromoted` calls — never
   `LoadGraph` — so the as-of envelope cited per SOLO-04 §5.4
   is meaningful. A target repo's staging is never read across
   the repo boundary.

`GraphReader` is the answer to the question "where does the
staging↔promoted merge live?" — it lives here, by name, and the
test surface for the merge rule is one `_test.go` next to the
implementation.

## 5. Composition root

`newDaemon` in `cmd/veska-daemon/wire.go` is the only place
dependencies are materialised. No mode flags. No conditional
adapter selection. It reads top to bottom (sketch — actual
constructor names live in `wire.go` / `providers.go`):

```go
func newDaemon(ctx context.Context, cfg Config) (*Daemon, error) {
    // 1. Logger.
    log := slog.New(...)

    // 2. SQLite pools + migrations. The handles live inside
    //    the sqlite adapter package; wire.go sees only ports.
    pools, err := sqliteinfra.OpenPools(cfg.DBPath)   // readDB + writeDB.hot + writeDB.embed
    if err != nil { return nil, err }
    if err := sqliteinfra.Migrate(pools); err != nil { return nil, err }

    // 3. Repositories. Each adapter owns the pools it needs; *sql.DB
    //    handles never cross into application/ or core/.
    graphRepo := sqliteinfra.NewGraphRepository(pools)
    taskRepo := sqliteinfra.NewTaskRepository(pools)
    findingRepo := sqliteinfra.NewFindingRepository(pools)
    repoRepo := sqliteinfra.NewRepoRepository(pools)
    embStore := sqliteinfra.NewEmbeddingStore(pools)
    vecIndex := sqliteinfra.NewVectorIndex(pools)
    postPromotionQueueRepo := sqliteinfra.NewPostPromotionQueueRepository(pools)  // PostPromotionQueueDrainer's read/write port

    // 4. Substrate.
    parser := treesitter.New()
    embProv := ollama.NewEmbedder(cfg.OllamaURL, cfg.EmbedModel)
    llm := ollama.NewGenerator(cfg.OllamaURL, cfg.LLMModel)
    watcher := fs.NewWatcher()
    tokens := charsdiv4.New()

    // 5. Application services.
    staging := application.NewStagingArea(parser)
    ingester := application.NewIngester(graphRepo, embStore, vecIndex, staging, log)
    graphReader := application.NewGraphReader(graphRepo, staging)  // §4.4a — owns the staging-overlay merge

    // 6. post-promotion queue drain goroutines (one per work_kind). Takes
    //    ports, not handles — the adapter behind postPromotionQueueRepo holds
    //    the *sql.DB.
    drains := application.StartPostPromotionQueueDrains(ctx, postPromotionQueueRepo, embProv, embStore, vecIndex, log)

    // 7. RPC router and Unix-socket listener. The top-level router
    //    composes the MCP and Control sub-routers and implements
    //    ports.RPCHandler. The MCP sub-router holds a *GraphReader,
    //    never *GraphRepository directly — every graph read goes
    //    through the overlay merge.
    mcpRouter := mcp.NewRouter(graphReader, taskRepo, findingRepo, vecIndex, tokens)
    ctlRouter := control.NewRouter(repoRepo, postPromotionQueueRepo, embProv, log)
    rpcRouter := application.NewRPCRouter(mcpRouter, ctlRouter)  // implements ports.RPCHandler
    listener, err := uds.Listen(cfg.SocketPath, rpcRouter)
    if err != nil { return nil, err }

    return &Daemon{listener, drains, watcher, log}, nil
}
```

`*sql.DB` is created and held inside `infrastructure/sqlite/`.
Application code holds ports (`PostPromotionQueueRepository`,
`GraphRepository`, etc.), never the raw handle. The lint
analyser in §6 enforces this on `core/`; the `cmd/` binary
package is the only place permitted to import the sqlite adapter
package and must pass the resulting ports — not the handle —
into `application/`.

That is the wiring. Reading it tells you what the daemon does.

The CLI (`cmd/veska`) and MCP shim (`cmd/veska-mcp`) have their
own much smaller wiring inside their respective `cmd/`
sub-packages. The shim's job is to proxy stdio frames to the
daemon's Unix socket; the CLI loads config and dials the socket.

### 5.1 Test composition

The daemon package has a test helper that takes a struct of
overrides; tests pass fakes for any port they want to control
and everything else defaults to the real impl pointing at a temp
directory. There is no DI magic; the helper is just `newDaemon`
with parameter substitution (`wire_test.go`).

## 6. Lint enforcement

One custom analyser, mandatory:

| Rule | Check |
|---|---|
| `layercheck` | `core/` imports nothing from `application/` or `infrastructure/` (covers both `core/domain/` and `core/ports/`). **`application/` imports nothing from `infrastructure/`** — application code depends on ports in `core/ports/`, not on concrete adapters. `application/` may import only `core/ports/` and `core/domain/`. No allow-list, no carve-out. |

`layercheck` is the only architectural lint. The standard
`golangci-lint` set covers the rest.

`golangci-lint` runs the standard suite (`govet`, `staticcheck`,
`gosimple`, `ineffassign`, `unused`, `gofmt`, `revive`). Race
detector is on for `make test`.

## 7. Why manual DI

- Missing dependencies surface as build errors.
- One file shows the wiring.
- Tests call constructors with fakes; no DI container to learn.
- Idiomatic Go.

The cost is some verbosity in `cmd/veska-daemon/wire.go`.
Acceptable.

## 8. Adding a port

A new port lands when:

1. A new aggregate root is added to SOLO-04 (rare; needs ADR).
2. A new substrate concern emerges (e.g. a second LLM provider —
   then `LLMGenerator` gets a second impl, no new port).
3. A new infrastructure abstraction is needed for testability.

The bar is "would this benefit from being mockable in a unit
test?" If yes, port. If no, plain code.

## 9. Open questions

- **OQ-S009:** Does the post-promotion-queue-drain goroutine model hold under
  refactor storms (e.g. 50k symbols promoted at once)? The drain
  must stay independent of the hot path; M2 spike measures.

(Canonical definitions live in `design/15-glossary/open-questions.md`.)
