-- solov2-7w1t — User-defined repo aliases. A separate table (not a column)
-- so multiple aliases per repo are allowed and the relationship stays
-- reversible without rewriting the parent row.
--
--   name     The human-typed alias. PRIMARY KEY enforces global uniqueness
--            across all repos; resolving an alias always picks one repo,
--            never an ambiguous list.
--   repo_id  Resolves to a row in repos. ON DELETE CASCADE drops aliases
--            with the underlying repo so an evict-and-re-register cycle
--            does not leave dangling pointers.
CREATE TABLE repo_aliases (
    name     TEXT PRIMARY KEY,
    repo_id  TEXT NOT NULL,
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);

CREATE INDEX idx_repo_aliases_repo_id ON repo_aliases(repo_id);
