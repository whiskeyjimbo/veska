CREATE TABLE nodes (
    node_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    repo_id          TEXT NOT NULL,
    language         TEXT NOT NULL,
    kind             TEXT NOT NULL,
    symbol_path      TEXT NOT NULL,
    file_path        TEXT NOT NULL,
    line_start       INTEGER,
    line_end         INTEGER,
    content_hash     TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (node_id, branch)
);
CREATE INDEX idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX idx_nodes_symbol ON nodes(symbol_path);
CREATE INDEX idx_nodes_content_hash ON nodes(content_hash);

CREATE TABLE edges (
    edge_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    repo_id          TEXT NOT NULL,
    src_node_id      TEXT NOT NULL,
    dst_node_id      TEXT NOT NULL,
    kind             TEXT NOT NULL,
    confidence       TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (edge_id, branch)
);
CREATE INDEX idx_edges_src ON edges(src_node_id, branch, kind);
CREATE INDEX idx_edges_dst ON edges(dst_node_id, branch, kind);

CREATE TABLE post_promotion_queue (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    promotion_id  TEXT NOT NULL,
    repo_id       TEXT NOT NULL,
    branch        TEXT NOT NULL,
    git_sha       TEXT NOT NULL,
    work_kind     TEXT NOT NULL,
    payload       TEXT NOT NULL,
    state         TEXT NOT NULL,
    attempts      INTEGER NOT NULL DEFAULT 0,
    enqueued_at   INTEGER NOT NULL,
    completed_at  INTEGER,
    error         TEXT
);
CREATE INDEX idx_post_promotion_queue_state ON post_promotion_queue(state, work_kind, seq);

CREATE TABLE node_embeddings (
    content_hash  TEXT PRIMARY KEY,
    model         TEXT NOT NULL,
    dim           INTEGER NOT NULL,
    embedding     BLOB NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE node_embedding_refs (
    node_id       TEXT PRIMARY KEY,
    content_hash  TEXT,
    state         TEXT NOT NULL,
    enqueued_at   INTEGER NOT NULL,
    embedded_at   INTEGER
);
CREATE INDEX idx_node_embedding_refs_state ON node_embedding_refs(state, enqueued_at);

CREATE VIRTUAL TABLE node_fts_words USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    words,
    tokenize = "unicode61 remove_diacritics 2"
);

CREATE VIRTUAL TABLE node_fts_trigrams USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    raw,
    tokenize = "trigram"
);
