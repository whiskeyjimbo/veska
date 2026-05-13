CREATE TABLE database_meta (
    key       TEXT PRIMARY KEY,
    value     TEXT NOT NULL,
    set_at    INTEGER NOT NULL
);

CREATE TABLE daemon_state (
    key       TEXT PRIMARY KEY,
    value     TEXT NOT NULL,
    set_at    INTEGER NOT NULL
);

CREATE TABLE repos (
    repo_id          TEXT PRIMARY KEY,
    root_path        TEXT NOT NULL UNIQUE,
    added_at         INTEGER NOT NULL,
    active_branch    TEXT,
    last_promoted_sha  TEXT,
    module_path      TEXT
);

CREATE TABLE nodes (
    node_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    content_hash   TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX idx_nodes_symbol ON nodes(symbol_path);
CREATE INDEX idx_nodes_content_hash ON nodes(content_hash);

CREATE TABLE edges (
    edge_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    src_node_id    TEXT NOT NULL,
    dst_node_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,
    confidence     TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (edge_id, branch),
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (dst_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE
);
CREATE INDEX idx_edges_src ON edges(src_node_id, branch, kind);
CREATE INDEX idx_edges_dst ON edges(dst_node_id, branch, kind);
CREATE INDEX idx_edges_repo_branch ON edges(repo_id, branch);

CREATE TABLE cross_repo_edge_stubs (
    stub_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    src_node_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,
    module_path    TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    language       TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (stub_id, branch),
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_stubs_src ON cross_repo_edge_stubs(src_node_id, branch);
CREATE INDEX idx_stubs_resolver ON cross_repo_edge_stubs(language, module_path, symbol_path);
CREATE INDEX idx_stubs_repo_branch ON cross_repo_edge_stubs(repo_id, branch);
