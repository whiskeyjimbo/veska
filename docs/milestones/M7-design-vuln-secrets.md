---
title: "M7 design prep — Vuln + Secrets scanning"
status: design
last_reviewed: 2026-05-19
related: [SOLO-05, SOLO-07, SOLO-11]
---

# M7 design prep — Vuln + Secrets Scanning

Implementation design for the two promotion checks that `SOLO-11 §2.1`
marks **PLANNED**: `vuln-scan` and `secrets-scan`. This doc fills in the
implementation detail behind the already-ratified design in
`SOLO-05 §2.2/§2.3` and `SOLO-11 §2.1`; it is the basis for the M7
milestone doc and its beads epic.

Supersedes the deferred placeholder bead `solov2-s5c.11`, which bundled
both checks as a single P4 backlog item.

## 1. Why this exists

`internal/application/checks/` ships only `deadcode` + `contractdrift`.
The `VulnSource` port exists but has only a `NullVulnSource` no-op
adapter; there is no `SecretsScanner` port at all. So no vuln or secrets
finding can ever surface today. `SOLO-02 US-05.01` is `planned` for this
reason. This was a deliberate roadmap deferral, not an oversight — M7
closes it.

## 2. What the design already commits us to

| Source | Commitment |
|---|---|
| `SOLO-05 §2.2` | `VulnSource` default impl `osv`: OSV.dev over HTTP, cache in `~/.veska/cache/osv/`, refresh goroutine on a 24h cadence. Promotion-time scan reads the cache only — network touched solely by the refresher. Port shape: `Refresh()` + `Scan([]Dependency) []VulnFinding`. Ships **off by default**. |
| `SOLO-05 §2.3` | `SecretsScanner` default impl `veska-builtin`: in-process regex rules + Shannon-entropy, redaction built in. Port shape: `Scan(ScanInput) []SecretFinding`. Ships **on by default**. |
| `SOLO-11 §2.1` | Both are synchronous promotion checks. Vuln-scan: input = dependency set, output = `vuln` findings (advisory ID, package, range). Secrets-scan: input = diff hunks (changed lines only), output = `secret_leak` findings (rule + redacted snippet). Neither touches the network at promotion time. |
| `domain.Finding` | `SourceLayer` enum already includes `security`. `checks.Check` (`Name()` + `Run(Input)`) is stable and ready for two more checks. |

## 3. Locked decisions

| Decision | Choice | Rationale |
|---|---|---|
| Ecosystem (first cut) | Go / `go.mod` only | npm/`package.json` a later follow-up; keeps the epic honest |
| Version resolution | Parse `go.mod` via `golang.org/x/mod/modfile` | Toolchain-free, deterministic, ~exact for tidied modules |
| Dependency scope | All — direct **and** indirect | A CVE in an indirect dep still affects you |
| Cold start | Silent catch-up | vuln-scan no-ops until the first `Refresh()` completes; promotion never network-blocks |
| Diff seam | Extend `checks.Input` with `AddedLines` | The promotion path already has the diff; keeps every check a pure function of `Input` |
| Vuln finding anchor | `FilePath = go.mod`, `key = advisoryID+package` | External modules are not graph nodes; `go.mod` is a real file; idempotent — bump the dep and the finding revalidates closed |
| Secrets finding anchor | `FilePath = leaking file`, `key = rule+line` | — |
| Source layer | Both → `LayerSecurity` | A leaked secret and a vulnerable dep are both security findings |
| Default posture | `VulnSource` off · `SecretsScanner` on | Vuln needs network egress → privacy-pillar opt-in; secrets is in-process and safe on by default. Per-check enable/disable lives in `[promotion]` config; port-impl selection is separate. |

## 4. Port shapes

```go
// ports/vulnsource.go — RESHAPED from the current Advisories(ctx, pkg) form
type Dependency  struct { Ecosystem, Name, Version string }
type VulnFinding struct { AdvisoryID, Package, AffectedRange, Severity, Summary string }

type VulnSource interface {
    // Refresh writes the advisory cache. Network egress happens here only;
    // called by the daemon's refresher goroutine, never on the promotion path.
    Refresh(ctx context.Context) error
    // Scan matches deps against the on-disk cache. No network.
    Scan(ctx context.Context, deps []Dependency) ([]VulnFinding, error)
}

// ports/secretsscanner.go — NEW
type Line          struct { Number int; Text string }
type ScanInput     struct { AddedLines map[string][]Line } // path -> newly-added lines
type SecretFinding struct { Rule, FilePath string; Line int; Redacted string; Confidence float64 }

type SecretsScanner interface {
    Scan(in ScanInput) ([]SecretFinding, error)
}
```

`checks.Input` grows one field; existing checks ignore it:

```go
type Input struct {
    RepoID, Branch, GitSHA string
    FilePaths  []string
    AddedLines map[string][]Line // NEW — populated once by the promotion path
}
```

## 5. Task DAG — one epic, two tracks

### Track A — Vuln scanning

| ID | Task | Depends on |
|---|---|---|
| A1 | Reshape `ports.VulnSource`; add `Dependency`/`VulnFinding`; update `NullVulnSource` | — |
| A2 | `go.mod` manifest reader → `[]Dependency` (`modfile`) | — |
| A3 | OSV adapter — `Refresh()` downloads OSV's full Go-ecosystem advisory dump (`osv-vulnerabilities.storage.googleapis.com/Go/all.zip`) to `~/.veska/cache/osv/`; `Scan()` matches deps against the local dump, offline | A1 |
| A4 | Refresh goroutine — daemon-owned, 24h cadence, kicked on start | A3 |
| A5 | `vulnscan` check — `go.mod` in `FilePaths` → read deps → `Scan` → emit `vuln` findings | A1, A2, A3 |
| A6 | Wiring: `wire.go`, `[vuln_source] provider="osv"`, egress allow-list, `veska doctor egress` | A3, A5 |

### Track B — Secrets scanning

| ID | Task | Depends on |
|---|---|---|
| B1 | `checks.Input.AddedLines` seam + promotion path populates it | — |
| B2 | `ports.SecretsScanner` + `ScanInput`/`SecretFinding` | — |
| B3 | `veska-builtin` impl — regex ruleset + Shannon-entropy + redaction | B2 |
| B4 | `secretsscan` check — emits `secret_leak` findings | B1, B2, B3 |
| B5 | Wiring: `wire.go` (on by default), `[promotion]` per-check enable/disable | B4 |

## 6. Exit gates

Every gate maps to a task — no orphan gates (the M5 lesson).

| # | Gate | Task |
|---|---|---|
| 1 | A `go.mod` with a known-CVE'd dependency produces a `vuln` finding in `eng_list_findings` | A5 |
| 2 | A commit adding a secret-shaped string produces a `secret_leak` finding with a redacted snippet | B4 |
| 3 | Vuln-scan touches no network at promotion time (verified) | A3, A5 |
| 4 | Secrets-scan flags only added lines, never pre-existing ones | B1, B4 |
| 5 | `veska doctor egress` reports the OSV endpoint when `[vuln_source]` is configured | A6 |
| 6 | `make all` clean against the new tree | all |

## 7. Follow-ups explicitly out of M7 scope

- npm / `package.json` ecosystem support for vuln-scan.
- Second `VulnSource` impls (GHSA, KEV, EPSS, internal feed).
- Second `SecretsScanner` impls (gitleaks, trufflehog shell-outs).
- Precise build-list version resolution via `go list -m all`.

## 8. Doc updates this milestone will require

- `SOLO-11 §2.1` — flip `secrets-scan` and `vuln-scan` from PLANNED to SHIPPED.
- `SOLO-02 US-05.01` — flip `planned` → `shipped`; remove the "Not yet shipped" note.
- `docs/operations/CONFIG-SURFACE.md` — document the `[vuln_source]` section and the `[promotion]` per-check toggles.
