-- solov2-kxo5.2 - Cache-tier columns on repos, prerequisite for accepting
-- remote git URLs in `veska repo add` and `veska search --repo`. See
-- solov2-kxo5 body + "Pinned design (2026-05-26)" notes for the full
-- design; this file only adds storage.
--
--   kind             tracked (today's behaviour, default) vs ephemeral
--                    (cloned into the cache tier on first URL query, subject
--                    to LRU eviction). Existing rows are 'tracked' via the
--                    DEFAULT - no behavioural change for already-registered
--                    repos.
--   canonical_url    Normalised git URL alias. On `repo add <path>` it is
--                    populated from `git remote get-url origin` (kxo5.4) so
--                    a later `search --repo <url>` against the same code
--                    resolves to the existing tracked row instead of cloning
--                    a duplicate. Ephemeral rows set it from the cloned URL.
--                    Partial unique index enforces "one row per canonical
--                    URL" without blocking NULLs (most existing rows).
--   last_accessed_at LRU eviction signal. Bumped on any query that touches
--                    an ephemeral row (kxo5.8). Tracked rows never read it.
--   prompted_at      Gates the once-per-row-lifetime acceptance prompt
--                    after `search --repo <url>`. NULL = eligible. The
--                    column persists across re-clones via the deterministic
--                    URL-derived repo_id; only an evict-and-re-clone resets
--                    it (the new row is genuinely new).
ALTER TABLE repos ADD COLUMN kind             TEXT NOT NULL DEFAULT 'tracked';
ALTER TABLE repos ADD COLUMN canonical_url    TEXT;
ALTER TABLE repos ADD COLUMN last_accessed_at INTEGER;
ALTER TABLE repos ADD COLUMN prompted_at      INTEGER;

CREATE UNIQUE INDEX idx_repos_canonical_url
    ON repos(canonical_url)
    WHERE canonical_url IS NOT NULL;
