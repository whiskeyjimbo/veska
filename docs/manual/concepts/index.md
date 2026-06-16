# Concepts

Veska is a **local code-intelligence daemon**. It parses your repo into a
graph, embeds that graph semantically, and exposes both through MCP so your
editor and your AI agent reason from the same model of the codebase. The data
lives in `~/.veska/` on your machine — no upstream, no shared service, no
multi-tenant tier.

These pages give you the mental model an operator needs. They are deliberately
lighter than the design set under `docs/design/`; follow the cross-links there
when you want the binding detail.

- **[The code graph](graph.md)** — how your repo becomes nodes and edges.
- **[Promotion & staging](promotion-staging.md)** — the two clocks: structural
  recall on save, semantic recall on commit.
- **[Semantic search & embeddings](embeddings.md)** — the in-process embedder
  and the lexical fallback.
- **[Finding duplicate & similar code](duplicates.md)** — exact, structural
  (renamed), and near tiers, repo-wide and cross-repo.
- **[Daemon topology](daemon-topology.md)** — one binary, three personalities,
  and what the daemon owns.

!!! abstract "The whole product, in three boxes"
    One daemon (`veska-daemon`) owns one SQLite file under `~/.veska/`. Two thin
    clients talk to it: the `veska` CLI and the `veska-mcp` editor shim. The
    default embedder runs in-process, so the default config makes **no outbound
    connection at all**.
