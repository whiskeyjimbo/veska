-- closeSupersededByRuleSetDiff selects open findings for a single rule:
-- WHERE repo_id=? AND branch=? AND rule=? AND state='open'. The existing
-- idx_findings_repo_branch covers only (repo_id, branch), so the query
-- residual-scans every finding in the repo+branch to match one rule and its
-- open state. This runs during promotion checks whenever a ruleset diff closes
-- superseded findings, and the scan grows with a repo's accumulated findings.
-- This composite index lets the planner seek straight to one rule's open rows.
CREATE INDEX idx_findings_repo_branch_rule_state ON findings(repo_id, branch, rule, state);
