-- 0019: ADR-S0017 atomic node-identity migration (solov2-dchd).
--
-- Re-keys the code graph onto portable, content-derived identity: node_id and
-- nodes.file_path become functions of the repo-relative slash path (§1, shipped
-- in the parser) and a portable repo-identity tier (§2, migration 0018). The
-- graph is a pure function of (source, identity scheme), so the cheapest correct
-- migration is drop + cold-rescan rather than an in-place re-key across 7
-- referencing tables.
--
-- The expensive artifact — embeddings — SURVIVES: node_embeddings is PK'd on
-- content_hash (body-derived, invariant under the identity change), so the
-- rescan re-points node_embedding_refs to existing vectors without re-embedding.
-- node_embeddings is therefore deliberately NOT cleared here.
--
-- The _dchd_old_* snapshot tables are read by the daemon's post-rescan identity
-- rescheme step (cli/daemon/rescheme.go) to carry user-authored suppressions
-- forward across the re-key, then dropped once the remap commits. They
-- intentionally outlive this migration.

-- 1. Snapshot the remap inputs BEFORE the drop. symbol_path is the parser node
--    name folded into node_id (promotion_store maps n.Name -> symbol_path), so
--    (repo_id, file_path, kind, symbol_path) is the node_id preimage and an
--    exact join key against the repopulated nodes.
CREATE TABLE _dchd_old_nodes AS
    SELECT DISTINCT node_id, repo_id, file_path, kind, symbol_path FROM nodes;
CREATE TABLE _dchd_old_findings AS
    SELECT finding_id, repo_id, branch, node_id, file_path, rule FROM findings;
CREATE TABLE _dchd_old_repos AS
    SELECT repo_id, root_path FROM repos;

-- 2. Drop the derived graph. edges + cross_repo_edge_stubs cascade from nodes
--    (FK ON DELETE CASCADE) but are cleared explicitly so this migration
--    self-documents what it clears and does not lean on cascade semantics
--    surviving a future schema edit.
DELETE FROM edges;
DELETE FROM cross_repo_edge_stubs;
DELETE FROM nodes;
DELETE FROM findings;
DELETE FROM node_embedding_refs;   -- node_id-keyed; rebuilt on rescan (content_hash survives)
DELETE FROM node_fts_words;        -- sink-maintained mirrors, not trigger-synced
DELETE FROM node_fts_trigrams;
DELETE FROM file_imports;          -- file-derived; repopulated on rescan
DELETE FROM post_promotion_queue;  -- holds stale node refs

-- 3. Force StartupResync's full-reparse branch (last_promoted_sha == "" path)
--    for every registered repo on next daemon boot, which repopulates the graph
--    under the new identity scheme.
UPDATE repos SET last_promoted_sha = NULL;
