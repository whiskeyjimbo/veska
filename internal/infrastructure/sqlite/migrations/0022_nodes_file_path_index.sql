-- File-path-scoped node queries filter on (repo_id, branch, file_path): the
-- per-file DELETE and prev-signature SELECT in promotion, DeleteFile's edge
-- subqueries, NodesInFile, and NodesForFile. The existing idx_nodes_repo_branch
-- covers only the first two columns, so each of those queries seeks to the
-- repo+branch and then residual-scans every node in it to match one file.
-- Promotion runs this once per changed file on every commit, so the scan grows
-- with repo size. This composite index lets the planner seek straight to a
-- single file's nodes.
CREATE INDEX idx_nodes_repo_branch_file ON nodes(repo_id, branch, file_path);
