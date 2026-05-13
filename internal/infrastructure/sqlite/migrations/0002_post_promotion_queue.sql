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
