-- solov2-izh6.31 - Persist the call_expression source line on CALLS edges
-- and cross-repo edge stubs.
--
-- Before: every Edge row recorded only (src_node_id, dst_node_id, kind),
-- so renderers fell back to the caller node's declaration line. A
-- function with N calls reported them all at the same line - the
-- junior-journey complaint on the cobra fixture where every cross-repo
-- edge attributed to `main in main.go:11` instead of the actual call
-- site inside each RunE closure body.
--
-- After: src_line carries the 1-indexed source line of the
-- call_expression. NULL means unknown (legacy rows, parsers that have
-- not adopted the field yet); the renderer falls back to today's
-- caller-line behaviour for those rows, so the migration is fully
-- backward-compatible - no in-place backfill needed. Newly-promoted
-- edges (post-migration) populate the column going forward; users see
-- accurate attribution after a `veska reindex` of any repo they want
-- updated.

ALTER TABLE edges ADD COLUMN src_line INTEGER;
ALTER TABLE cross_repo_edge_stubs ADD COLUMN src_line INTEGER;
