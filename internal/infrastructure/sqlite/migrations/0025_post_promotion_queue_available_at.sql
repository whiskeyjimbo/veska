-- A deferred row (handler returned ErrDeferWork) is held until available_at,
-- so the per-file auto_link gate can wait for a file's embeddings without
-- busy-looping the lowest-seq pending row or burning its retry budget.
-- 0 = immediately available, matching every existing row.
ALTER TABLE post_promotion_queue ADD COLUMN available_at INTEGER NOT NULL DEFAULT 0;
