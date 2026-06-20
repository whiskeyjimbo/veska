-- 0019: ADR-S0017 node-identity scheme change.
--
-- node_id and nodes.file_path now key on the repo-relative slash path (§1) and a
-- portable repo-identity tier (§2). The graph is a pure function of (source,
-- identity scheme), so the migration is simply: drop the derived graph and let
-- the startup resync cold-rescan every repo under the new scheme.
--
-- This is deliberately a PLAIN drop+rescan, not a key-preserving migration. The
-- scheme transition is one-time and runs while veska is single-user (ADR-S0017
-- sequences it BEFORE any DB is shared), so the only thing a rescan "loses" is a
-- handful of suppressions whose anchor id moved — cheap to re-author. A shared
-- DB never hits this: routine rescans are id-stable (content-derived ids), so
-- suppressions ride along untouched and need no carry-forward. See ADR-S0017
-- "Migration" for why the elaborate snapshot/re-key path was dropped.
--
-- The expensive artifact — embeddings — SURVIVES: node_embeddings is PK'd on
-- content_hash (body-derived, invariant), so the rescan re-points
-- node_embedding_refs to existing vectors without re-embedding. node_embeddings
-- is therefore deliberately NOT cleared. The suppressions table is likewise left
-- untouched: a suppression whose anchor id is unchanged stays valid; one whose
-- id moved simply dangles until re-authored.
DELETE FROM edges;
DELETE FROM cross_repo_edge_stubs;
DELETE FROM nodes;
DELETE FROM findings;             -- re-derived on rescan (checks re-run)
DELETE FROM node_embedding_refs;  -- node_id-keyed; rebuilt on rescan (content_hash survives)
DELETE FROM node_fts_words;       -- sink-maintained mirrors, not trigger-synced
DELETE FROM node_fts_trigrams;
DELETE FROM file_imports;         -- file-derived; repopulated on rescan
DELETE FROM post_promotion_queue; -- holds stale node refs

-- Force StartupResync's full-reparse branch (last_promoted_sha == "" path) for
-- every registered repo on next daemon boot.
UPDATE repos SET last_promoted_sha = NULL;
