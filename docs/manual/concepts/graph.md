# The code graph

Veska parses your repository with tree-sitter and stores it as a **graph**:
**nodes** (the things in your code) connected by **edges** (the relationships
between them). Everything else — search, blast radius, the wiki, promotion
checks — is a query over this graph.

## Nodes

A node is a single addressable thing: a function, method, type, file, route,
command, and so on. Every node carries enough to locate it in your source:

- a stable **id**,
- a **kind** (e.g. function, type, file),
- a **name**,
- a **file path** and **line range** (`line_start` / `line_end`).

When a tool returns a node, those fields are what let your editor or agent jump
straight to the definition — no guessing function names.

## Edges

An edge connects two nodes with a **kind** that names the relationship — a call
edge from one function to another, a containment edge from a file to the symbols
it defines, and so on. Edges are what make structural questions answerable:
"what calls this?", "what would break if I change this?", "where does control
enter this package?"

## Querying the graph

You rarely touch the graph directly. You query it through tools and commands:

| Want to… | Use |
|---|---|
| Find a symbol by name | `eng_find_symbol` / `veska symbol` |
| See a file's symbols | `eng_get_file_nodes` / `veska file-nodes` |
| Trace callers/callees | `eng_get_call_chain` / `veska calls` |
| Estimate change impact | `eng_get_blast_radius` / `veska blast` |
| Search by meaning | `eng_search_semantic` / `veska search` |

See the **[CLI reference](../reference/cli.md)** and
**[MCP tools reference](../reference/mcp-tools.md)** for the full surface.

!!! tip "Adding a node or edge kind"
    Kinds are open-ended (`nodes.kind` / `edges.kind` are unconstrained text),
    so new kinds need no database migration — they ripple through a handful of
    domain and query sites instead. This is an internals detail; see the design
    set if you're extending Veska.

Next: **[Promotion & staging](promotion-staging.md)** — how the graph stays
current as you edit and commit.
