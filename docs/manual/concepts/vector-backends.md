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

Measured across Go repositories spanning ~11k to ~113k symbols, each indexed in
isolation (model2vec embeddings, float32 index, deterministic serial build).
memvec's linear scan returns the exact nearest neighbours, so it is the **recall
baseline** - 1.0000 by definition - and usearch's recall is the fraction of those
exact matches it also returns.

| repo    | symbols | time to ready | usearch build | query p95 (memvec) | query p95 (usearch) | usearch recall | RAM (memvec) | RAM (usearch) |
|---------|--------:|--------------:|--------------:|-------------------:|--------------------:|---------------:|-------------:|--------------:|
| go-git  |  11,268 |           56s |         +1.5s |             3.7 ms |             0.32 ms |         0.9992 |       33 MiB |        65 MiB |
| veska   |  12,900 |           60s |         +2.5s |             4.6 ms |             0.43 ms |         0.9993 |       38 MiB |        65 MiB |
| grpc-go |  19,520 |          113s |         +3.1s |             6.8 ms |             0.41 ms |         0.9965 |       58 MiB |       129 MiB |
| consul  |  37,272 |          258s |         +7.5s |            11.7 ms |             0.56 ms |         0.9974 |      111 MiB |       129 MiB |
| tidb    | 113,043 |        27 min |          +29s |            47.6 ms |             0.64 ms |         0.9803 |      335 MiB |       532 MiB |

Three things the curve makes clear:

- **Query latency is usearch's whole point, and the gap widens with size.**
  memvec's per-query cost climbs linearly (3.7 → 47.6 ms p95) while usearch stays
  essentially flat (0.32 → 0.64 ms) - a **74x** gap at the largest repo. That is
  O(n) linear scan versus O(log n) graph search.
- **Recall holds, then bends.** usearch stays at ~0.997+ up through ~37k symbols,
  then dips to **0.98 at 113k** - the HNSW approximation showing its cost right
  where you'd actually reach for usearch. Still high, but no longer ~1.0.
- **Time to ready is the same for both backends.** It's dominated by *embedding*,
  which is backend-independent (56s → 27 min as the repo grows). usearch adds only
  a small index-build premium on top (1.5s → 29s); memvec adds essentially none.
  So "memvec is ready faster" is technically true but negligible.

!!! note "What the columns mean"
    - **Symbols** - functions, types, methods, and code chunks; a repo's "size" in Veska terms. 
    - **Time to ready** - the one-time, backend-independent cost to parse and embed the repo so it's searchable.
    - **usearch build** - the extra one-time cost to construct usearch's HNSW graph on top (memvec's is sub-second).
    - **Query p95** - the 95th-percentile latency of a single nearest-neighbour lookup, which is what semantic search and auto-linking do per call.
    - **Recall** - of the truly-closest matches, the fraction usearch returns versus memvec's exact result.
    - **RAM** - the resident footprint of the vector index alone.

## Which should I use?

- **Small-to-medium repos (under ~40k symbols): `memory`.** It is the default -
  exact, zero-setup, lowest RAM, and single-digit-millisecond queries are
  imperceptible. Most repositories never need anything else.
- **Large repos, or latency-sensitive multi-repo setups: `usearch`.** Once
  linear-scan latency becomes noticeable - it's ~12 ms at 37k symbols and ~48 ms
  at 113k - usearch's flat query time pays off. Set `VESKA_VECTOR_BACKEND=usearch`;
  it requires the `hnsw_native` build and `libusearch_c.so` on the loader path, or
  the daemon refuses to start. Above ~100k symbols expect ~98% recall, not ~100%.

!!! note "usearch is not the low-memory option"
    Because usearch stores full float32 vectors (the same precision as the durable
    `node_embeddings`) *plus* an HNSW graph, it uses **more** RAM than memvec, not
    less - roughly 1.5-2x. memvec is the RAM-saver; usearch buys query speed.

Next: **[Daemon topology](daemon-topology.md)**.
