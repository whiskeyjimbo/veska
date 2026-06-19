-- Persist parsed file imports.
--
-- Until now, the importer table was transient: the parser computed
-- file.Imports (local-alias → import-path) per file, the promotion handler
-- used it during stub creation, and then it was dropped. That meant
-- `veska deps list` could only show modules with *resolved* calls (entries
-- in cross_repo_edge_stubs) and missed modules that were imported but
-- only referenced via struct literals / type assertions / interface
-- implementations — the failure mode flagged in the junior
-- journey (cobra absent before `go mod vendor`).
--
-- This table records every import the parser saw, scoped to the same
-- (repo_id, branch, file_path) grain as nodes. PromotionStore deletes
-- rows on per-file re-promotion (mirroring the nodes/edges pattern) so
-- removed imports vanish from the index in the same commit.
--
-- alias is the local identifier when the import has one
-- (e.g. `import foo "github.com/x/foo"` → alias="foo"); empty otherwise
-- so a future caller that needs the alias-only view doesn't have to
-- re-parse.
CREATE TABLE file_imports (
    repo_id          TEXT NOT NULL,
    branch           TEXT NOT NULL,
    file_path        TEXT NOT NULL,
    import_path      TEXT NOT NULL,
    alias            TEXT NOT NULL DEFAULT '',
    language         TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch, file_path, import_path),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX idx_file_imports_module ON file_imports(repo_id, branch, import_path);
