-- solov2-dchd.1 (ADR-S0017 §2) - Persist the portable identity anchor a repo
-- resolved to at `repo add` time, so node IDs derive from a globally-shared
-- anchor (committed content / remote URL) rather than the local checkout path,
-- and so the choice is auditable and never silently re-derived per client.
--
--   identity_tier    Which rung of the fallback chain this repo resolved to:
--                    'module-hostpath' (Go host/path module or scoped npm -
--                    converges AND globally unique; the supported shared-DB
--                    anchor), 'origin-url' (canonical git remote - unique but
--                    diverges across forks), 'module-bare' (vanity/bare module
--                    name - local-stable, collision-prone), or 'abs-root'
--                    (absolute checkout path - never converges, local-only).
--                    NULL on rows registered before this migration; backfilled
--                    by the identity-scheme rescan (solov2-dchd.4).
--   identity_anchor  The exact string hashed into repo_id for the chosen tier
--                    (module path, canonical URL, or canonical abs root).
--                    Stored for auditability and deterministic migration.
ALTER TABLE repos ADD COLUMN identity_tier   TEXT;
ALTER TABLE repos ADD COLUMN identity_anchor TEXT;
