-- Persist a Type-2 (renamed-variable) clone signal on each node so the
-- duplicates surface can cluster structurally-identical code that ContentHash
-- (byte-identical only) misses.
--
-- structural_hash is a SHA-256 over the node's identifier-/literal-normalised
-- token stream (computed at parse time from the AST — see
-- internal/infrastructure/treesitter/structural.go). Two functions collide here
-- when they have the same shape after renaming locals/params/literals, even if
-- their verbatim text (and thus content_hash) differs.
--
-- Nullable: packages/imports/chunks and any non-Go node the parser does not
-- structurally hash carry NULL and never participate in structural grouping.
-- Legacy rows are NULL until `veska reindex` backfills them (the parser sets
-- the value; the node upsert refreshes it on conflict). Fully backward-
-- compatible: the column is nullable and existing writers that omit it leave
-- it NULL.
ALTER TABLE nodes ADD COLUMN structural_hash TEXT;

CREATE INDEX idx_nodes_structural_hash ON nodes(structural_hash);
