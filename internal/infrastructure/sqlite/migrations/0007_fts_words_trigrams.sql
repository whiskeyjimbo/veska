-- Replaces the original `node_fts` virtual table (migration 0004) with a
-- two-index lexical fallback per SOLO-08 §3.3:
--   - node_fts_words    : built-in unicode61 tokenizer over a pre-tokenised
--                          column. The promoter splits camelCase / snake_case
--                          / `::` / `.` in Go (internal/infrastructure/sqlite/
--                          tokenize.Symbol) and writes the joined output here.
--                          This is the precise-match arm.
--   - node_fts_trigrams : built-in trigram tokenizer over the raw
--                          (kind + symbol_path + name) string. Substring and
--                          fuzzy-typo matches surface here.
--
-- m3.03.2 fuses results from both with Reciprocal Rank Fusion at query time.
-- See ADR amendment in docs/design/08-data-and-storage §3.3.
DROP TABLE IF EXISTS node_fts;

CREATE VIRTUAL TABLE node_fts_words USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    words,
    tokenize = "unicode61 remove_diacritics 2"
);

CREATE VIRTUAL TABLE node_fts_trigrams USING fts5(
    node_id        UNINDEXED,
    branch         UNINDEXED,
    repo_id        UNINDEXED,
    raw,
    tokenize = "trigram"
);
