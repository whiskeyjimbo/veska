# Semantic search & embeddings

`eng_search_semantic` (CLI: `veska search`) answers natural-language questions
over your code - "where do we parse config?", "the retry logic for uploads." It
works by **embedding** every symbol into a vector and finding the symbols whose
vectors are closest to your query's.

## The in-process embedder

By default Veska embeds with **model2vec** (`potion-code-16M`), a fast static
*code* embedder that runs **in-process** - microseconds per symbol, no network,
no separate service. With the fat binary (`make build`) the weights are compiled
in; nothing to install.

Veska **elects one embedder at boot** and never mixes vector spaces - exactly
one embedder owns the index at a time. The election order:

1. **model2vec** - the default and recommended choice.
2. **static-v2** - an in-binary fallback that works with no model files at all
   (lower quality), used only when model2vec is unavailable.

Ollama is **optional** and only backs the LLM review pipeline; it is not used
for embeddings in the default config. (Power users can force an Ollama embedding
model, but model2vec is faster and higher-quality on code.) See
**[Install](../getting-started/install.md#the-embedder)**.

## Eventual consistency & the lexical fallback

Semantic search runs on the **commit clock** (see
**[Promotion & staging](promotion-staging.md)**): a newly promoted symbol is
searchable once the post-promotion queue embeds it. With the default embedder
that lag is usually seconds.

During the lag window, `eng_search_semantic` does **not** return nothing - it
falls back to a **BM25 lexical index** over symbol names and tags the response:

- `degraded_reasons: ["embedding_pending"]` - embeddings are still catching up;
- `degraded_reasons: ["embedder_offline_lexical_fallback"]` - the embedder is
  unavailable, serving lexical results only.

So you always get an answer; the `degraded_reasons` field tells you whether it's
the full semantic ranking or the lexical stand-in. Check `eng_get_status`'s
`pending_embeds` count to see how far behind embedding is.

!!! note "Scores are relative, not absolute"
    `eng_search_semantic` returns a post-fusion RRF score (it blends the vector
    and lexical rankings). Top scores cluster in a narrow band by construction
    and are only meaningful **relative to other hits in the same query** - don't
    threshold on the absolute number.

Next: **[Daemon topology](daemon-topology.md)**.
