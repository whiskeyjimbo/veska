# Install

## Requirements

- **Go 1.26+** (Veska builds from source; it uses cgo for SQLite and
  tree-sitter).
- **No external services for core use.** SQLite, the vector index, and the
  default embedder all run in-process. A fresh machine indexes and searches
  with nothing else installed or running.

## Build

`make build` produces the **fat binary** by default — the model2vec embedder
weights are compiled into the binary, so the install is zero-setup: no separate
download, no network, no fallback at boot.

```sh
make build        # default: ~104 MB fat binary. Zero setup at runtime.
make build-small  # ~42 MB thin binary (veska, veska-daemon, veska-mcp).
                  # Size-sensitive only (CI, containers); you must then run
                  # `veska install model2vec` to avoid the low-quality
                  # static-v2 fallback at boot.
```

Binaries land in `./bin/`. Either add them to your `PATH`:

```sh
export PATH="$PWD/bin:$PATH"
```

…or use the `./bin/` prefix in the commands throughout this manual.

## Install into your `PATH`

After a `make build`, drop the binaries into a user bin directory:

```sh
make install                                         # → ~/.local/bin (default)
VESKA_INSTALL_DIR=/usr/local/bin sudo make install   # system-wide
```

For a self-contained tarball (the three fat binaries + `install.sh` + a
README), run `make release-archive`. The archive lands at
`dist/veska-<version>-<os>-<arch>.tar.gz`.

## The embedder

Semantic search needs an embedder. Veska **elects one at boot** in preference
order — it never mixes vector spaces, so exactly one embedder owns the index at
a time:

1. **model2vec** (`potion-code-16M`) — a fast, in-process static *code*
   embedder. The default and recommended choice.
    - **Fat binary** (`make build`) — compiled in. Nothing to install.
    - **Thin binary** (`make build-small`) + `veska install model2vec` — a
      one-time ~62 MB download into `~/.veska/`.
2. **static-v2** — an in-binary fallback that works with no model files at all
   (lower quality). Used only when model2vec is unavailable.

No Ollama, no network, and no separate process is required for search.

## Optional: Ollama

Ollama is **only** for the optional LLM review pipeline (off by default). It is
**not** used for embeddings in the default config.

```sh
# macOS:        brew install ollama && ollama serve &
# Linux (snap): sudo snap install ollama && ollama serve &
# Linux (curl): curl -fsSL https://ollama.com/install.sh | sh && ollama serve &
```

Next: **[Quickstart](quickstart.md)**.
