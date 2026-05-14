-- Adds signature + prev_signature columns to nodes for contract-drift detection.
--
-- `signature` is the current declaration signature of the symbol (function,
-- method, interface). It is populated by the parser layer via
-- domain.Node.Signature; the Promoter threads it onto each inserted row.
--
-- `prev_signature` captures the signature value of the matching (node_id,
-- branch) row immediately before the promotion that wrote the current row.
-- The Promoter SELECTs the prior signature inside the promotion transaction
-- (BEFORE the per-file DELETE) and writes it back into the new row, so a
-- single SQL row carries both halves of the comparison.
--
-- Both columns are nullable so first-time promotions (no prior row) and nodes
-- of kinds that have no meaningful signature (e.g. files, fields) remain
-- representable.
ALTER TABLE nodes ADD COLUMN signature      TEXT;
ALTER TABLE nodes ADD COLUMN prev_signature TEXT;
