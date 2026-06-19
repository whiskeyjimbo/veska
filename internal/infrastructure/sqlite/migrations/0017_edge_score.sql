-- Persist the auto-link similarity score on SIMILAR_TO edges so
-- near-duplicate clustering can threshold them directly.
--
-- Before: edges recorded only (src, dst, kind, confidence). The similarity
-- score autolink computed (Hit.Score, in the 1/(1+L2^2) space documented on
-- internal/application/autolink) was dropped at SaveEdges time and survived
-- only as text inside the auto-link finding message ("(score %.2f)"). That
-- made "give me SIMILAR_TO pairs above a HIGHER threshold than autolink's
-- related cutoff" (near-duplicate detection) impossible without re-running a
-- similarity pass or parsing finding prose.
--
-- After: score carries that value on the edge itself. NULL means unknown
-- (legacy rows, or any non-SIMILAR_TO edge — only autolink populates it).
-- The autolink writer refreshes it on conflict (ON CONFLICT ... DO UPDATE SET
-- score = COALESCE(excluded.score, edges.score) in EdgeRepo.SaveEdges), so a
-- `veska reindex` backfills scores onto pre-existing edges and refreshes drift
-- — without touching confidence/last_promoted_at. Fully backward-compatible:
-- the column is nullable and non-SIMILAR_TO writers pass NULL.

ALTER TABLE edges ADD COLUMN score REAL;
