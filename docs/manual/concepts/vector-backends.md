# Vector storage backends

Embeddings (see **[Semantic search & embeddings](embeddings.md)**) are only useful
if Veska can find the *nearest* ones to a query quickly. That is the job of the
**vector backend**, chosen at daemon boot with `VESKA_VECTOR_BACKEND`:

- **`memory`** (`memvec`, the default) - an exact brute-force linear scan held in
  RAM. Simple, no native dependency, lowest memory, and always 100% accurate.
  Query time grows linearly with the number of symbols.
- **`usearch`** - an approximate **HNSW** graph (needs `libusearch_c.so` and the
  `hnsw_native` build). Query time stays roughly flat as the repo grows, in
  exchange for more RAM, a slower one-time index build, and ~99.9% (not 100%)
  recall.

## How they compare

Measured across Go repositories of increasing size (model2vec embeddings,
float32 index). memvec's linear scan returns the exact nearest neighbours, so it
is the **recall oracle** - 1.0000 by definition - and usearch's recall is the
fraction of those exact matches it also returns. Regenerate this table any time
with `make eval-backend-matrix`.

| repo    | symbols | query p95 (memvec) | query p95 (usearch) | usearch recall | RAM (memvec) | RAM (usearch) |
|---------|--------:|-------------------:|--------------------:|---------------:|-------------:|--------------:|
| go-git  |  11,262 |             4.0 ms |              0.3 ms |         0.9990 |       34 MiB |        65 MiB |
| veska   |  12,900 |             4.3 ms |              0.4 ms |         0.9994 |       39 MiB |        65 MiB |
| grpc-go |  19,520 |             6.2 ms |              0.3 ms |         0.9979 |       59 MiB |       129 MiB |
| consul  |  37,272 |            11.6 ms |              0.4 ms |         0.9965 |      113 MiB |       129 MiB |

The trend: **memvec's query latency climbs with repo size
(4 → 12 ms) while usearch stays flat (~0.3 ms)**. usearch pays for that with roughly 2x 
the RAM and a multi-second index build (versus memvec's milliseconds); recall holds above
99.6% throughout.

!!! note "What the columns mean"
    **Symbols** - functions, types, methods, and code chunks; a repo's "size" in
    Veska terms. **Query p95** - the 95th-percentile (slow-tail) latency of a
    single nearest-neighbour lookup, which is what semantic search and auto-linking
    do per call. **Recall** - of the truly-closest matches, the fraction usearch
    returns versus memvec's exact result. **RAM** - the resident footprint of the
    vector index alone.

## Which should I use?

- **Small-to-medium repos (under ~50k symbols): `memory`.** It is the default -
  exact, zero-setup, lowest RAM, and single-digit-millisecond
  queries are imperceptible. Most repositories never need anything else.
- **Large repos, or latency-sensitive multi-repo setups: `usearch`.** Once
  linear-scan latency becomes noticeable - tens of milliseconds, or auto-linking
  across a large graph - usearch's flat query time pays off. Set
  `VESKA_VECTOR_BACKEND=usearch`; it requires the `hnsw_native` build and
  `libusearch_c.so` on the loader path, or the daemon refuses to start.

!!! note "usearch is not the low-memory option"

Next: **[Daemon topology](daemon-topology.md)**.
