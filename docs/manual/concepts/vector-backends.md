# Vector storage backends

Embeddings (see **[Semantic search & embeddings](embeddings.md)**) are only useful
if Veska can find the *nearest* ones to a query quickly. That is the job of the
**vector backend**, selected at daemon boot via `VESKA_VECTOR_BACKEND`:

- **`memory`** (`memvec`) - an exact brute-force linear scan held in RAM. Simple,
  no native dependency, lowest memory, and always 100% accurate. Query time grows
  linearly with the number of symbols.
- **`usearch`** - an approximate **HNSW** graph (needs `libusearch_c.so` and the
  `hnsw_native` build). Query time stays roughly flat as the repo grows, in
  exchange for more RAM, a slower one-time index build, and ~99% (not 100%)
  recall at large scale.
- **`auto`** (the default when `VESKA_VECTOR_BACKEND` is unset) - picks `memvec`
  for small graphs and `usearch` once the largest indexed `(repo, branch)` crosses
  ~10k vectors, *if* usearch is compiled in. You usually don't set this at all.

## How they compare

Measured across Go repositories spanning ~11k to ~220k symbols, each indexed in
isolation (model2vec embeddings, float32 index, default serial build). memvec's
linear scan returns the exact nearest neighbours, so it is the **recall
baseline** - 1.0000 by definition - and usearch's recall is the fraction of those
exact matches it also returns.

| repo    | symbols | time to ready | usearch build | query p95 (memvec) | query p95 (usearch) | usearch recall | RAM (memvec) | RAM (usearch) |
|---------|--------:|--------------:|--------------:|-------------------:|--------------------:|---------------:|-------------:|--------------:|
| go-git  |  11,333 |           41s |         +1.4s |             4.8 ms |             0.34 ms |         0.9992 |       34 MiB |        65 MiB |
| veska   |  13,080 |           58s |         +2.5s |             5.4 ms |             0.52 ms |         0.9999 |       40 MiB |        65 MiB |
| grpc-go |  19,520 |           90s |         +2.8s |             6.8 ms |             0.37 ms |         0.9974 |       59 MiB |       129 MiB |
| consul  |  37,272 |        3m 45s |         +6.9s |            13.9 ms |             0.49 ms |         0.9966 |      113 MiB |       129 MiB |
| vault   |  38,800 |        3m 33s |         +7.9s |            15.4 ms |             0.50 ms |         0.9980 |      118 MiB |       129 MiB |
| tidb    | 113,055 |          13 min |        +24.4s |            35.6 ms |             0.60 ms |         0.9885 |      343 MiB |       532 MiB |
| k8s     | 220,369 |          35 min |        +51.8s |                n/a |             0.57 ms |            n/a |          n/a |     1,080 MiB |

The `k8s` row is **usearch-only**: at 220k symbols an exact in-RAM memvec index is
impractical (which is exactly the regime usearch exists for), so its memvec
columns and the usearch recall (measured *against* memvec) are `n/a`. The point it
makes on its own: **usearch query latency stays flat (0.57 ms p95) even at 220k -
the same sub-millisecond it posts at 11k** - while its index costs ~1 GB of RAM
and a ~52 s one-time build.

Three things the curve makes clear:

- **Query latency is usearch's whole point, and the gap widens with size.**
  memvec's per-query cost climbs linearly (4.8 → 35.6 ms p95 at 113k) while usearch
  stays essentially flat (0.34 → 0.60 ms) - a **~59x** gap at tidb, and usearch
  holds that same sub-millisecond latency (0.57 ms) at **220k**, where building an
  exact memvec index is no longer practical at all. That is O(n) linear scan versus
  O(log n) graph search.
- **Recall holds, then bends.** usearch stays at ~0.997+ up through ~39k symbols,
  then dips to **~0.988 at 113k** - the HNSW approximation showing its cost right
  where you'd reach for usearch. Still high, but no longer ~1.0 (and the build
  profile below can buy most of it back).
- **Time to ready is the same for both backends.** It's dominated by *embedding*,
  which is backend-independent (41s → 13 min as the repo grows). usearch adds only
  a small index-build premium on top (1.4s → 24s); memvec adds essentially none.
  So "memvec is ready faster" is technically true but negligible.

!!! note "What the columns mean"
    - **Symbols** - functions, types, methods, and code chunks; a repo's "size" in Veska terms.
    - **Time to ready** - the one-time, backend-independent cost to parse and embed the repo so it's searchable.
    - **usearch build** - the extra one-time cost to construct usearch's HNSW graph on top (memvec's is sub-second).
    - **Query p95** - the 95th-percentile latency of a single nearest-neighbour lookup, which is what semantic search and auto-linking do per call.
    - **Recall** - of the truly-closest matches, the fraction usearch returns versus memvec's exact result.
    - **RAM** - the resident footprint of the vector index alone.

## Tuning the usearch build: profiles

usearch's HNSW recall decays as the graph grows, so a single construction setting
no longer fits every repo size. The `[storage] usearch_index_profile` config lever
(`default` | `fast` | `balanced` | `accurate`) trades **index build time against
recall**. It does **not** change query latency - the search beam is fixed, so the
numbers in the backend table above apply to every profile.

Each cell below is `build time / recall` (recall vs the exact memvec oracle).
`fast` and `balanced` build in parallel and are nondeterministic, so their figures
are the median of repeated runs.

| repo    | symbols | default (serial ef64) | fast (parallel ef64) | balanced (parallel ef128) | accurate (serial ef192) |
|---------|--------:|----------------------:|---------------------:|--------------------------:|------------------------:|
| go-git  |  11,333 |        1.4s / 0.9992 |       0.6s / 0.9983 |            1.5s / 0.9993 |          5.3s / 0.9999 |
| veska   |  13,080 |        2.5s / 0.9999 |       1.2s / 0.9964 |            2.4s / 0.9991 |          8.4s / 1.0000 |
| grpc-go |  19,520 |        2.8s / 0.9974 |       1.3s / 0.9971 |            2.9s / 0.9990 |         10.5s / 0.9997 |
| consul  |  37,272 |        6.9s / 0.9966 |       3.2s / 0.9953 |            7.1s / 0.9987 |         25.2s / 0.9998 |
| vault   |  38,800 |        7.9s / 0.9980 |       3.5s / 0.9955 |            7.9s / 0.9983 |         27.5s / 0.9997 |
| tidb    | 113,055 |       24.4s / 0.9885 |      12.0s / 0.9791 |           27.4s / 0.9941 |         97.3s / 0.9990 |
| k8s     | 220,369 |         51.8s / n/a |                   - |             51.5s / n/a |                      - |

(k8s recall is `n/a` - no exact memvec oracle at that size - and only `default`
and `balanced` were built. Note `balanced` (parallel, ef128) lands at essentially
the same wall-clock as `default` (serial, ef64) even at 220k, so the wider beam is
effectively free there; on a graph this large `balanced` is the clear pick.)

What the profiles are for:

- **Below ~40k symbols, it barely matters** - every profile builds in seconds at
  ~0.995+ recall. Leave it on `default`.
- **The trade only bites at scale.** At tidb (113k), `default` loses recall
  (0.9885) while `accurate` recovers it (0.9990) for ~4x the build time, and
  `balanced` recovers most of it (0.9941) at about the same wall-clock as
  `default` (the parallel build offsets the wider beam). `fast` is the cheapest
  build but the lowest recall (0.9791).
- **Rule of thumb at large scale:** `balanced` is the sweet spot (recall back near
  ~0.994 for roughly the default's build time); `accurate` when you want maximum
  recall and a *reproducible* graph and can spend the build time; `fast` only when
  rebuild speed dominates and a recall dip is acceptable.

## Which should I use?

For most setups, **leave `VESKA_VECTOR_BACKEND` unset and let `auto` decide.** It
keeps small repos on exact, zero-setup `memvec` and switches to `usearch` once a
repo crosses ~10k vectors (where linear-scan latency starts to show) - but only if
the binary was built with `hnsw_native` and `libusearch_c.so` is on the loader
path. If usearch isn't available, `auto` stays on `memvec` rather than failing.

Override only when you have a reason:

- **Force `memory`** to stay exact (1.0000 recall) and lowest-RAM regardless of
  size - e.g. a recall-critical workflow on a mid-size repo.
- **Force `usearch`** to get flat query latency below the auto threshold, or to
  make the requirement explicit. Note: an *explicit* `usearch` refuses to start if
  the native library is missing (unlike `auto`, which degrades to `memvec`).

On a large repo, pair `usearch` with a `usearch_index_profile` (above) to manage
the build-time-vs-recall trade.

!!! note "usearch is not the low-memory option"
    Because usearch stores full float32 vectors (the same precision as the durable
    `node_embeddings`) *plus* an HNSW graph, it uses **more** RAM than memvec, not
    less - roughly 1.5-2x. memvec is the RAM-saver; usearch buys query speed.

!!! note "About these numbers"
    Measured on a 4-core Intel i7-7700 with the in-binary model2vec embedder and a
    float32 usearch index, one repo at a time. Absolute times are host-specific and
    illustrative; the *shapes* - memvec's linear query growth, usearch's flat
    latency, and the build-vs-recall trade - are what carry across machines.
    Regenerate with `tools/loadtest/usearchab/backend-matrix.sh`.

Next: **[Daemon topology](daemon-topology.md)**.
