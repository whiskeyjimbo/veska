-- Mark intra-module (internal) package imports in file_imports.
--
-- file_imports historically stored only EXTERNAL module imports (used by
-- `veska deps list`); intra-module imports were filtered out at promotion.
-- They are now persisted too, flagged internal=1, so a package-level
-- dependency graph can be aggregated from per-file rows for import-cycle
-- and layering checks. Keeping the grain per-file (the existing
-- delete-by-file + reinsert maintenance) means removed imports vanish on
-- re-promotion without recomputing whole packages.
--
-- Existing rows are all external by construction, so DEFAULT 0 is correct
-- and no backfill is needed; the next promotion repopulates internal rows.
ALTER TABLE file_imports ADD COLUMN internal INTEGER NOT NULL DEFAULT 0;

-- Index the internal subset for the package-dependency aggregator.
CREATE INDEX idx_file_imports_internal ON file_imports(repo_id, branch, internal);
