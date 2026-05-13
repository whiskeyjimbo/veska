---
id: SOLO-05
title: "Plugin Surface — Go Interfaces, One Impl Each"
status: draft
version: 0.1.0
last_reviewed: 2026-05-08
related: [SOLO-01, SOLO-04, SOLO-08, SOLO-11]
---

# SOLO-05 — Plugin Surface

Engram has plugin-swappable ports. Each is a plain Go interface
in `internal/core/ports/`. **Each ships with exactly one
implementation at M1.** A second impl is a future ADR — once
that ADR ratifies the second impl, provider-keyed selection
(`[<port>].provider = "x" | "y"`) becomes the legitimate
composition mechanism without further ceremony. Today's
CONFIG-SURFACE.md already names four `provider` keys (§1.1
below); some of those second impls land in M3+ as their
respective ADRs ratify them.

This file is the inventory and a paragraph each on the eleven
plugin-swappable ports. That is the whole contract.

**Two port inventories, one truth.** SOLO-07 §4 catalogues all
19 Go interfaces in `core/ports/`: 4 repository ports, 2
storage adjuncts, 12 substrate ports, and 1 driving port
(`RPCHandler`, SOLO-07 §4.3a). The substrate group is split:
**11 plugin-swappable** (Embedder, LLMGenerator, Tracker,
VulnSource, SecretsScanner, Notifier, CoverageSource,
OwnershipSource, FileWatcher, CodeParser, TokenEstimator) and
**1 non-swappable substrate primitive** (Logger). Logger is a
port for testability (a fake captures structured records in
unit tests) but is not "swappable" in the plugin sense: a
second impl would just be a different slog handler, configured
at the bootstrap call site, not selected by config.
`RPCHandler` is the inbound seam the UDS transport adapter
calls into; a second transport is a second driving adapter, not
a second `RPCHandler`. SOLO-07 is the canonical port catalogue;
SOLO-05 covers the 11 plugin-swappable ports below.

## 1. The shape of a port

A port is a Go interface. The composition root constructs the one
configured impl at daemon start and passes it into the
application layer through the constructor. There is no registry.
There is no name lookup. There is no cardinality enum. The
application code just holds a field of the interface type.

```go
// internal/bootstrap/wire.go (sketch)
func buildApp(cfg Config) (*App, error) {
    embedder := ollama.New(cfg.Ollama)
    tracker  := bd.New(cfg.Bd)
    vuln     := osv.New(cfg.Osv)
    // ...
    return &App{
        Embedder: embedder,
        Tracker:  tracker,
        Vuln:     vuln,
        // ...
    }, nil
}
```

Adding a second impl of any port is a code change in `wire.go` plus
a config-driven switch. The first time we want that switch is the
first time we write a second impl. Until then, the field is just
the concrete type behind the interface.

### 1.1 Configuration

Ports that have operator-visible config use a flat top-level
TOML section, not a `[plugins.<port>]` nesting. Four ports have
config surface; CONFIG-SURFACE.md §3 is the canonical inventory:

| Port | TOML section | Provider key |
|---|---|---|
| `Embedder` | `[embedder]` | `provider = "ollama"` |
| `LLMGenerator` | `[llm_generator]` | `provider = "ollama"` |
| `Tracker` | `[tracker]` | `provider = "none" (default) \| "bd-cli"` |
| `VulnSource` | `[vuln_source]` | `provider = "osv" \| "ghsa"` |

The other six ports in §2 (`SecretsScanner`, `Notifier`,
`CoverageSource`, `OwnershipSource`, `FileWatcher`, `CodeParser`)
have **no config surface** — the daemon constructs the one shipped
impl unconditionally. If a future impl needs config, the ADR that
adds it also adds the TOML section to CONFIG-SURFACE.md.

If `provider` names the one impl shipped for the port, the
daemon constructs it. If it doesn't, the daemon refuses to start
with a clear error. That refusal is the entire registry
mechanism.

## 2. Port inventory

Eleven ports (the original ten plus `TokenEstimator`, §2.11).
Some have a default impl that ships and runs by default;
some have a default impl of "off / no-op" because the feature is
opt-in. The "what a second impl would need to provide" column is
not a contract — it is a sketch of where another impl might go.

| Port | Default impl | Status |
|---|---|---|
| `Tracker` | `none` (no tracker integration); `bd-cli` (the local issue-tracker CLI) is the only shipped non-`none` impl | Ships **off** by default — opt-in via `engram init` or `[tracker] provider = "bd-cli"` |
| `VulnSource` | `osv` (OSV.dev with a local cache) | Ships off by default |
| `SecretsScanner` | `engram-builtin` (in-process regex + entropy) | Ships on by default |
| `Embedder` | `ollama` with `nomic-embed-text` | Ships on by default |
| `LLMGenerator` | `ollama` | Ships off by default (needed for review pipeline) |
| `Notifier` | `stderr` (writes to daemon log) | Ships on by default |
| `CoverageSource` | none | No default impl |
| `OwnershipSource` | `codeowners` (parses `.github/CODEOWNERS`) | Ships on by default |
| `FileWatcher` | `fsnotify` | Ships on by default |
| `CodeParser` | `tree-sitter` (Go, TS, TSX, JS, JSX) | Ships on by default |
| `TokenEstimator` | `chars/4` heuristic | Ships on by default |

### 2.1 `Tracker`

Reads the active task list and accepts task-state writes. **The
default is `none`** — no tracker integration. Auto-link's
"recent activity" signal (SOLO-11 §4) falls back to local
heuristics (branch name, recently-touched symbols) when no
tracker is configured.

The only shipped non-`none` impl is `bd-cli`, which shells out
to the local `bd` CLI binary, parses its JSON output, and
watches the on-disk issue file for changes. **The daemon does
not bundle `bd`.** When a user opts into `bd-cli`, `engram init`
probes for `bd` on `$PATH` (the same shape as the Ollama probe
in SOLO-03 §3.2) and, if missing, prints the install command for
the host platform and exits non-zero. We do not silently degrade
a tracker integration the user thinks is on.

A second impl (Linear, Jira, GitHub Issues) maps its IDs and
states onto the `TrackerIssue` struct below.

#### `TrackerIssue` shape

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | Tracker-native ID (e.g. `engram-42`). Stable for the issue's lifetime. |
| `title` | string | yes | Short summary. |
| `type` | `task \| bug \| feature \| epic` | yes | Closed enum; mirrors `bd --type`. Other trackers map their type onto this set; novel types fail closed (`task`). |
| `priority` | int 0-4 | yes | 0=critical, 4=backlog. Mirrors `bd --priority`. Other trackers map `P0..P4` or "high/medium/low" onto this range. |
| `status` | `open \| in_progress \| blocked \| closed` | yes | Closed enum. Trackers with richer state machines collapse onto these. |
| `labels` | `[]string` | yes (may be empty) | Free-form. Used by auto-link's recent-activity signal. |
| `assignee` | `*string` | optional | Tracker-native assignee handle. |
| `parent` | `*string` | optional | Parent issue `id`. |
| `depends_on` | `[]string` | yes (may be empty) | IDs this issue depends on. |
| `created_at` | RFC 3339 string | yes | UTC. |
| `updated_at` | RFC 3339 string | yes | UTC; used by auto-link recent-activity scoring (SOLO-11 §4). |

#### Port surface

```go
type Tracker interface {
    // Read
    List(ctx context.Context, q TrackerQuery) ([]TrackerIssue, error)
    Get(ctx context.Context, id string) (*TrackerIssue, error)

    // Active-task pin
    GetActive(ctx context.Context) (*TrackerIssue, error)
    SetActive(ctx context.Context, id string) error
    ClearActive(ctx context.Context) error

    // Write
    Create(ctx context.Context, in TrackerIssueDraft) (*TrackerIssue, error)
    Update(ctx context.Context, id string, patch TrackerIssuePatch) (*TrackerIssue, error)
    Close(ctx context.Context, id string, reason string) error
}

type TrackerQuery struct {
    Status      []string  // empty = any
    Type        []string  // empty = any
    Labels      []string  // matches if any label present
    UpdatedSince *time.Time
}

// TrackerIssueDraft and TrackerIssuePatch carry the same fields as
// TrackerIssue minus tracker-set fields (id, created_at, updated_at);
// patch fields are pointers so callers can omit unchanged values.
```

The full write surface ships now even though the deferred MCP
write tools (SOLO-09 §8.2) only consume it later. Locking the
interface means M2/M3 add tools, not new methods.

`SetActive` is the one method the application layer's auto-link
weights depend on (SOLO-11 §4). Trackers that have no native
"active" concept persist the pin in a side file; the `bd-cli`
impl uses `.beads/current_task` (CONFIG-SURFACE.md §1).

### 2.2 `VulnSource`

Returns advisories for a given dependency set. The default impl
talks to OSV.dev over HTTP, caches responses in
`~/.engram/cache/osv/`, and refreshes the cache on a configurable
cadence (default 24h). At promotion time the impl reads the cache; the
network is touched only by the refresh goroutine. A second impl
(GHSA, KEV, EPSS, internal feed) would need to provide
`Refresh()` (write to its own cache file) and
`Scan([]Dependency) []VulnFinding` (read from that cache and
return matched advisories).

### 2.3 `SecretsScanner`

Scans diff hunks (and optionally full files) for secret-shaped
strings. The default impl is in-process: a list of regex rules
plus a Shannon-entropy heuristic, with redaction built in. It is
fast enough to run on every promotion. A second impl (gitleaks,
trufflehog) would need to provide `Scan(ScanInput) []SecretFinding`
where `ScanInput` is a bundle of file snapshots and/or a diff;
typically it shells out to a child process and parses the
output. Per-rule confidence is the impl's responsibility.

### 2.4 `Embedder`

Turns text into a `[]float32` vector with a stable
`ModelVersion()`. The default impl posts to a local Ollama
endpoint with `nomic-embed-text` (768 dims). The post_promotion_queue
embedder goroutine is the only caller.

**Ollama is the only `Embedder` impl.** A remote embedder
(OpenAI `text-embedding-3-*`, Gemini `text-embedding-004`, etc.)
is a known second-impl candidate but does not ship today. The
reasons:

- **Privacy.** Remote embedding sends every node body to a third
  party. The "zero telemetry by default" pillar (PRODUCT.md)
  requires explicit opt-in for any such impl.
- **Cost.** Per-promotion cost recurring forever; refactor storms
  re-embed thousands of nodes. Real money, even if small.
- **Schema coupling.** Providers produce different vector
  dimensions (nomic = 768, Gemini = 768, OpenAI 3-small = 1536,
  OpenAI 3-large = 3072). `vec_nodes` has one dim per database;
  switching providers means rebuilding it. We parameterise the
  dim at `engram init` time (SOLO-08 §3.3) so this becomes a
  known migration path rather than a schema rewrite.

The trigger to land a second impl is M3 measurement: if Ollama
throughput on the reference laptop is so low that the product is
unusable without remote embeddings, the second impl ships under
an ADR with the measurement in hand. Any second impl provides
`Embed(text) Vector` and a stable `ModelVersion()` string; the
version string keys embeddings in SQLite and must change
whenever the underlying model or dim changes.

### 2.5 `LLMGenerator`

Runs a prompt and returns generated text. Used by the review
pipeline (SOLO-11 §3). The default and only impl talks to local
Ollama with a configurable model.

Hosted providers (Anthropic, OpenAI, Gemini, OpenAI-compatible)
are deferred behind an ADR + measurement: ship the local impl,
run it on the reference workload, then add hosted backends with
the local numbers in hand.

The interface is shaped to accept a second impl without redesign:
`Generate(Prompt) Generation` returns text plus a `Provenance`
record `(model_id, prompt_template_version, input_hash)` so cached
outputs invalidate when the model changes. Streaming is optional;
non-streaming is what the review pipeline uses.

The only `provider` value the daemon accepts today is `ollama`;
any other value fails fast at startup. Hosted-provider config
(API keys, additional `provider` values, USD cost caps) lands
alongside hosted impls — not earlier.

M5 ships the `ollama` impl. See `milestones/M5.md` epic m5.00.

### 2.6 `Notifier`

Pushes a finding-arrived notification somewhere. The default impl
writes a structured line to the daemon's stderr; the editor sees
findings via MCP, so a noisy notifier is unnecessary for the
common case. A second impl (Slack, email, webhook, syslog) would
need to provide `Notify(NotificationEvent) error` and decide
whether to fan out by severity / kind on its own — there is no
central de-dup policy in the daemon. Notifier is fire-and-forget
from the application layer's perspective; failures log and do not
block.

### 2.7 `CoverageSource`

Ingests a coverage report (Go cover, lcov, cobertura, etc.) and
emits per-symbol coverage records joined onto the graph. No
default impl ships — coverage is opt-in and the user wires it via
`engram coverage import <path>` or a CI hook. A second impl would
need to parse its format and return `CoverageRecord` rows keyed
by `(file_path, line_start, line_end)`; the application layer
resolves those back to `NodeID`s.

### 2.8 `OwnershipSource`

Answers "who owns this file?" The default impl parses
`.github/CODEOWNERS` (and the equivalents under
`.gitlab/CODEOWNERS` and `docs/CODEOWNERS`) and returns the
matched owner strings as-is — no resolution to a directory record,
because there is no directory (SOLO-10 §5). A second impl
(an org-specific YAML, an internal API) would need to provide
`Owners(path) []string` and a `Refresh()` to re-read its source.

### 2.9 `FileWatcher`

Emits file-change events on the channel the save pipeline reads.
The default impl is a thin wrapper around `fsnotify` with the
recursive-watch logic that Go's standard fsnotify doesn't have.
A second impl (polling for filesystems where fsnotify is
unreliable, or a remote watcher for a future networked variant)
would need to provide `Events() <-chan FileEvent` and respect
`.engramignore`. There is no pluggable kernel-event API; the
interface is the channel.

### 2.10 `CodeParser`

Parses a file into a list of nodes and edges. The default impl
uses tree-sitter with grammars for Go, TypeScript, TSX,
JavaScript, JSX. A second impl (LSP-based, language-server-as-a-
parser, a different tree-sitter grammar set) would need to
provide `Parse(path, content) ParsedFile` returning the same
shape: a list of symbol nodes with kinds (function, type, file,
package, method, field) and the edges between them (CALLS,
IMPORTS, CONTAINS, TESTS, DEPENDS_ON). New languages add grammars
to the existing impl rather than swap impls.

The `symbol_path` formats per language are normative (SOLO-04
§5.1.1). TSX/JSX/JS share the TypeScript/JavaScript format; they
are tree-sitter grammar variants of the same `symbol_path`
namespace, not separate language address spaces. ADR-S0006's
"five edges × N languages" framing counts TSX/JSX/JS once each
because they each carry their own grammar; the address space is
shared with TS/JS. New OO languages (Java, Python, C#) carry
OQ-S008 — the five-edge set may need IMPLEMENTS / EXTENDS, and
that lands as an ADR against measured queries when those
parsers ship.

### 2.11 `TokenEstimator`

Estimates the token count of a string. Used by the MCP token
budget (SOLO-09 §4.3) and the review-pipeline daily/per-commit
caps (SOLO-11 §3.1). Counts as the **eleventh** plugin-swappable
port (so SOLO-05's table grows to 11 when this lands; the §4
"swappable" count adjusts in lockstep).

```go
type TokenEstimator interface {
    Estimate(s string) int
    ModelHint() string  // e.g. "chars/4" or "cl100k_base"; logged in audit
}
```

Default impl is `chars/4` — the cheapest correct-shape estimator
for English-ish source code. It is documented as approximate;
callers tune token caps with that shape in mind. Adding a real
tokenizer (tiktoken's `cl100k_base`) is a future second-impl
that ships with measurement showing the chars/4 estimate is too
loose for a real workload. The `ModelHint()` is recorded in the
audit line for any tool whose response was truncated against a
token cap, so callers can interpret cap behaviour against the
estimator that produced it.

## 3. What we explicitly aren't building

This list is normative: re-introducing any of these is a
redesign, not a feature.

- **Capability schemas as build artifacts.** No
  `<Port>Capabilities` struct that gets serialized into a manifest
  and consulted at runtime. If a behavior varies between impls,
  the interface either has the method or it doesn't; the code
  branches on impl-specific knowledge in the composition root,
  not on a runtime capability lookup.
- **Per-port lint analysers.** No `tools/lint/<port>capability`
  vetting that every method has a documented capability flag. The
  Go compiler is the contract checker.
- **A typed registry generic.** No `GetSingle[S any](r
  PluginRegistry, ctx, port PortID)`. The composition root holds
  fields of concrete interface types and passes them in. There is
  no registry to look up; if you have the App, you have the
  Embedder.
- **Cardinality enums.** No `single` vs `additive` taxonomy.
  Where multiple things should run (e.g. multiple `VulnSource`
  feeds when we add a second impl), the application layer holds
  a slice and iterates. That is not a port property; it is just
  Go.
- **Mode-conditional registries.** No `[L]` / `[W]` / `[C]`
  switches. There is one mode: the local daemon.
- **Routing dispatchers.** No "executor profile" enum, no
  per-pass dispatcher, no profile-to-impl matrix. Background work
  runs as goroutines in this process. That's the executor.
- **An `Executor` port at all.** Background work is `go func()
  { ... }`. There is no port to swap.
- **The `ExternalSource` substrate.** No central `Fetch[Req,
  Resp]` port, no global rate limiter, no cross-port egress
  allow-list, no `external_cache` table. Each port impl that
  needs HTTP imports `net/http`, talks to its endpoint, and
  caches into a file under `~/.engram/cache/<port>/`. If we ever
  want a uniform egress posture, it can be added later as a
  shared HTTP client; it is not a contract.

The reason for cutting all of these: each one was scaffolding for
a second consumer that does not yet exist. The first impl is
the only impl. When a second impl shows up, we will have a real
case to design against, and an ADR is the right place to record
the resulting contract.

## 4. Adding a port

Three steps:

1. Write the Go interface in `internal/core/ports/`.
2. Write the one impl in `internal/infrastructure/<port>/`.
3. Wire it into `internal/bootstrap/wire.go`.

That is the whole process. No ADR is required for the first
impl; the design lives in the interface comment and the impl
lives in the package. An ADR is required when a second impl
arrives, because that is when the interface needs to harden into
a contract.

## 5. Diagnostics

`engram doctor plugins` reports:

- Each port's configured `kind`.
- Whether the impl initialized cleanly at startup.
- The last successful operation timestamp (for impls that have a
  natural "last call" — Embedder, VulnSource refresh, Tracker
  list).
- The last error, if any.

That is the entire plugin-diagnostics surface. There is no
`platform_describe` MCP tool; the editor doesn't need to discover
what plugins are loaded because it doesn't talk to plugins
directly.
