---
id: ADR-S0016
title: "Rename the in-memory vector backend (sqlitevec → memvec)"
status: Accepted
date: 2026-06-01
deciders: [whiskeyjimbo]
supersedes: []
extends: [ADR-S0015]
related: [ADR-S0014, ADR-S0015]
verified: false
---

# ADR-S0016 - Rename the in-memory vector backend (sqlitevec → memvec)

Naming correction for the default VectorStorage backend. No behavioural change.

## Status

Accepted

## Context

The default vector backend lived in `internal/infrastructure/vector/sqlitevec/`,
exported `SQLiteVecStore`, was selected by `BackendSQLiteVec` / the config value
`vector_backend = "sqlite-vec"`, and was described throughout the code as the
"sqlite-vec backend".

That name is a misnomer. The package contains **zero SQL**: it is a pure-Go
in-memory map with a brute-force L2 linear scan (`store.go`). The name was a
vestige of the abandoned asg017/sqlite-vec extension spike - the spike hit a red
ceiling at ~100k vectors and the project pivoted to the dual-backend design in
[[ADR-S0014]] / [[ADR-S0015]]. The brute-force path survived the pivot as the
zero-dependency default, but kept the spike's name.

The cost was concrete: the name misled an AI agent investigating driver options
into believing sqlite-vec was a live production dependency. [[ADR-S0015]]'s own
context compounded this by calling it "the sqlite-vec CGo dependency [that]
compiles from source with no external .so" - there is no such dependency; the
store is an in-memory Go map.

## Decision

Rename the backend to describe what it is - an in-memory vector store:

- Package `internal/infrastructure/vector/sqlitevec` → `…/memvec` (`git mv`,
  history preserved). Type `SQLiteVecStore` → `Store`; constants
  `SQLiteVecYellowThreshold` / `SQLiteVecRedThreshold` → `YellowThreshold` /
  `RedThreshold`.
- `vector.BackendSQLiteVec` → `vector.BackendMemory`; the selector value
  `"sqlite-vec"` → `"memory"` (env `VESKA_VECTOR_BACKEND`, config
  `vector_backend`, default in `config.DefaultConfig`).
- No back-compat alias: this is a clean break. veska is still pre-release with no
  external consumers, the daemon does not yet read the toml `vector_backend`
  value, and a stray `"sqlite-vec"` value now fails fast at startup with
  `unknown VESKA_VECTOR_BACKEND "sqlite-vec" (want "memory" or "usearch")` - a
  clear one-line fix. (The bead originally specified a one-release deprecation
  alias; dropped as unnecessary complexity given the development stage.)
- The `tools/loadtest/spikes/sqlitevec/` tree is **left untouched** - that path
  is historically accurate (it really was the sqlite-vec extension spike) and is
  still imported by the `hnsw_native`-tagged vector benchmark.

## Consequences

- No runtime behaviour change: same in-memory map, same linear scan, same
  thresholds. The selector value changes: `vector_backend = "sqlite-vec"` /
  `VESKA_VECTOR_BACKEND=sqlite-vec` must be updated to `"memory"` (it now errors
  at startup instead of silently resolving).
- The dual-backend strategy of [[ADR-S0015]] is unchanged - only the default
  backend's name. Where S0015 calls it the "sqlite-vec backend" / "sqlite-vec
  CGo dependency", read "in-memory (memvec) backend"; that framing was the same
  misnomer this ADR corrects.
- Historical references to the *real* asg017/sqlite-vec extension (the spike,
  M0/M1 milestones, ADR-S0001/S0014/S0015 context) remain accurate and are not
  rewritten.
