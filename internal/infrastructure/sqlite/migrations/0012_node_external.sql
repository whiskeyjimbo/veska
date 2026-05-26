-- solov2-bchl phase 1 — Mark nodes that came from a registered repo's
-- vendored or module-cache dependencies (third-party Go code) so the
-- read path can label them and the write path can scope re-indexes
-- without touching first-party rows.
--
-- A vendored module is identified by the (repo_id, file_path-prefix)
-- pair: file_path under <root>/vendor/<module-path>/ means external.
-- The flag is set at insert time by the external indexer (see
-- internal/application/extindex). Existing rows get external=0 via the
-- DEFAULT, matching the prior single-tier behaviour.
ALTER TABLE nodes ADD COLUMN external INTEGER NOT NULL DEFAULT 0;

-- Index for queries that filter by external + repo_id (e.g.
-- 'show me my own symbols only' or 'list vendored deps'). Cheap on a
-- table this size; nodes is already indexed on (repo_id, branch).
CREATE INDEX idx_nodes_external ON nodes(repo_id, external);
