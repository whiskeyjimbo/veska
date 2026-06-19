-- Add a per-node source body so the +snippet embed-text
-- projection (faithful sweep, solov2-ok0) has actual code to project. The
-- nodes table previously stored only content_hash, which addresses the
-- embedding cache but is not itself projectable text.
--
-- Forward-only: existing rows get NULL. A NULL snippet is benign — the
-- projection treats it as an empty part, and domain.EmbedText skips empty
-- parts, so a node without a snippet simply degrades to the baseline
-- projection rather than failing.
ALTER TABLE nodes ADD COLUMN snippet TEXT;
