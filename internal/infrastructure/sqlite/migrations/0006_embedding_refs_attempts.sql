-- Adds a per-row retry counter to node_embedding_refs.
--
-- Forward-only ALTER TABLE: every existing row defaults to attempts=0.
-- The embedder worker bumps this counter on each Embed failure; when it
-- reaches the policy threshold (m3.02.3: 3) the worker flips state to
-- 'failed' and FetchPending stops returning the row.
ALTER TABLE node_embedding_refs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
