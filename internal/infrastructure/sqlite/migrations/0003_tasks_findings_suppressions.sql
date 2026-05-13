CREATE TABLE tasks (
    task_id       TEXT PRIMARY KEY,
    repo_id       TEXT NOT NULL,
    tracker       TEXT,
    tracker_ref   TEXT,
    title         TEXT NOT NULL,
    active        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX idx_tasks_active_one_per_repo ON tasks(repo_id) WHERE active = 1;

CREATE TABLE findings (
    finding_id    TEXT NOT NULL,
    branch        TEXT NOT NULL,
    repo_id       TEXT NOT NULL,
    node_id       TEXT,
    file_path     TEXT,
    severity      TEXT NOT NULL,
    source_layer  TEXT NOT NULL,
    rule          TEXT NOT NULL,
    message       TEXT NOT NULL,
    state         TEXT NOT NULL,
    closed_reason TEXT,
    created_at    INTEGER NOT NULL,
    closed_at     INTEGER,
    actor_id      TEXT NOT NULL,
    actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (finding_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_findings_state ON findings(state, severity);
CREATE INDEX idx_findings_anchor ON findings(node_id, branch);
CREATE INDEX idx_findings_repo_branch ON findings(repo_id, branch);

CREATE TABLE suppressions (
    suppression_id TEXT PRIMARY KEY,
    scope          TEXT NOT NULL,
    target         TEXT NOT NULL,
    branch         TEXT,
    rule           TEXT,
    reason         TEXT NOT NULL,
    expires_at     INTEGER,
    created_at     INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system'))
);
CREATE INDEX idx_suppressions_target ON suppressions(target, branch);
