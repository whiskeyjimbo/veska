# Vector storage - package notes

Dual-backend `ports.VectorStorage`, selected at startup by `VESKA_VECTOR_BACKEND`
(`memory` | `usearch`; empty → `memory`). Entry point: `NewVectorStorage` in
`backend.go`.

## `memory` (default - `memvec/`)

- Pure-Go in-memory map with a brute-force L2 linear scan. **Zero SQL.** The old
  name `sqlitevec` was a misnomer (renamed to `memvec`, ADR-S0016): it never
  loaded a sqlite-vec / `vec0` extension and there is no `vec_nodes` virtual table.
- Nothing persists. The index is rebuilt at `Daemon.Start` from the durable
  `node_embeddings` bytes via `application/embedder.RehydrateVectors`.
- Adequate below `memvec.YellowThreshold` (75k) nodes.

## `usearch` (optional scale backend)

- HNSW + float16, compiled behind the `hnsw_native` build tag; requires
  `libusearch_c.so` on the loader path at runtime.
- Persists to sibling `vec-{repoID}-{branch}-{modelID}.hnsw` files (+ a Go
  metadata sidecar), **not** inside `veska.db`. Also rehydrated from
  `node_embeddings` on boot.

## Refuse-to-start

- `Open()` (`open.go`) returns `ErrVectorStoreUnavailable` **only** when `usearch`
  is selected but the `hnsw_native` tag / `libusearch_c.so` is absent - the daemon
  then fails to start (`wire.go`: "open vector storage").
- The default `memory` backend has **no** native dependency and cannot fail this
  way. There is no `ErrSqliteVecMissing` / `ErrVecExtensionMissing` sentinel.

Design docs: SOLO-08 §1.1 / §3.3; ADR-S0014 / S0015 / S0016.
