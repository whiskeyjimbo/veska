-- Content-addressed embedding bytes.
CREATE TABLE node_embeddings (
    content_hash  TEXT PRIMARY KEY,
    model         TEXT NOT NULL,
    dim           INTEGER NOT NULL,
    embedding     BLOB NOT NULL,
    created_at    INTEGER NOT NULL
);

-- Per-node refs into the content-addressed store.
-- node_id conceptually references nodes(node_id) but nodes has a composite PK
-- (node_id, branch), making a strict FK unenforceable by SQLite.  The FK to
-- content_hash is enforced; the node_id relationship is maintained by the
-- application layer per design note SOLO-08.
CREATE TABLE node_embedding_refs (
    node_id       TEXT PRIMARY KEY,
    content_hash  TEXT,              -- NULL while pending
    state         TEXT NOT NULL,     -- pending|ready|failed
    enqueued_at   INTEGER NOT NULL,
    embedded_at   INTEGER,
    FOREIGN KEY (content_hash) REFERENCES node_embeddings(content_hash)
);
CREATE INDEX idx_node_embedding_refs_state ON node_embedding_refs(state, enqueued_at);

-- FTS5 lexical index on symbol_path and name.
CREATE VIRTUAL TABLE node_fts USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    symbol_path,
    name,
    tokenize = "unicode61 remove_diacritics 2"
);
