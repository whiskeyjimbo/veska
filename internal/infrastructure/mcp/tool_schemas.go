package mcp

import "encoding/json"

// Input schemas for every MCP tool. Without these, tools/list returns
// {properties:[], required:[]} for the tool catalog and MCP clients (Claude
// Code, Cursor, generic IDE adapters) have to discover argument names by
// guessing, parsing human error messages, or reading the daemon source.
// Publishing the schema makes the surface self-describing (solov2-jtl5.9).
//
// Schemas are draft 2020-12. Field descriptions match the param-struct comments
// in their respective tool files; aliases that the handler accepts (e.g.
// file_path/path on eng_get_file_nodes) are listed under "properties" so
// callers know either form is valid.
//
// Every schema sets "additionalProperties": false so unknown keys are
// rejected with -32602 at dispatch (solov2-9bzq). Tools whose handler
// resolves the active repo from the caller's working directory must list
// "cwd" explicitly — the dispatch-time validator only knows what's in
// "properties".
//
// New tools should add their schema here and reference it from the ToolSpec.

var addRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "root_path": {"type": "string", "description": "Absolute filesystem path to the git repository root."}
  },
  "required": ["root_path"]
}`)

var removeRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id": {"type": "string", "description": "Full repo_id (SHA-256 hex) or short_id prefix returned by eng_list_repos."}
  },
  "required": ["repo_id"]
}`)

var setRepoAliasInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Bind a human-friendly alias to a repo. repo_id may be any form that resolves: full id, short_id, an existing alias, or an unambiguous prefix (>= 4 chars).",
  "properties": {
    "name":    {"type": "string", "description": "The alias to set. Rejected if hex-only and >= 4 chars (would shadow the prefix resolver) or if it contains whitespace."},
    "repo_id": {"type": "string", "description": "Repo to bind. Resolved via the standard repo_id progression."},
    "force":   {"type": "boolean", "description": "Overwrite an existing alias pointing at a different repo. Default false."}
  },
  "required": ["name", "repo_id"]
}`)

var removeRepoAliasInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "name": {"type": "string", "description": "The alias to remove. Errors if unknown so a typo is loud."}
  },
  "required": ["name"]
}`)

var promoteRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Re-promote the latest commit of a registered repo. One of repo_id or root_path is required; when both are passed, repo_id wins. branch, git_sha, and actor_kind/actor_id are optional overrides for callers (e.g. agents) that want to attribute the promotion to themselves or pin a specific SHA; when omitted the handler reads HEAD from git and stamps the system actor.",
  "properties": {
    "repo_id":    {"type": "string", "description": "Full repo_id or short_id prefix."},
    "root_path":  {"type": "string", "description": "Absolute filesystem path; canonicalised via EvalSymlinks before lookup."},
    "branch":     {"type": "string", "description": "Optional branch override; defaults to the repo's active_branch (or 'main')."},
    "git_sha":    {"type": "string", "description": "Optional commit SHA to promote at; defaults to git HEAD of the resolved root_path."},
    "actor_kind": {"type": "string", "enum": ["human", "agent", "system"], "description": "Attribution kind for the promotion stamp; defaults to 'system'. Must be paired with actor_id."},
    "actor_id":   {"type": "string", "description": "Attribution id (e.g. 'agent:claude', 'human:alice'); defaults to 'service:veska'. Must be paired with actor_kind."}
  }
}`)

var reindexRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Force a full cold-scan reparse of a registered repo, in-daemon. One of repo_id or root_path is required; when both are passed, repo_id wins. Returns when the scan completes; the daemon's standard 'cold scan: starting' / 'cold scan: complete' log pair is emitted as for the add-repo path.",
  "properties": {
    "repo_id":   {"type": "string", "description": "Full repo_id or short_id prefix."},
    "root_path": {"type": "string", "description": "Absolute filesystem path; canonicalised via EvalSymlinks before lookup."}
  }
}`)

var getCurrentRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "cwd": {"type": "string", "description": "Working directory to match against registered repo roots; if omitted the daemon uses the connecting client's reported cwd."}
  }
}`)

var listReposInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "No parameters; returns every registered repo with short_id, root_path, active_branch, and last_promoted_sha.",
  "properties": {}
}`)

var getRepoInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id": {"type": "string", "description": "Full repo_id or short_id prefix."}
  },
  "required": ["repo_id"]
}`)

var getStatusInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "No parameters; returns daemon-wide health (rollup status, pending_embeds, scans_in_flight, degraded_reasons).",
  "properties": {}
}`)

var getConfigInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "No parameters; returns the daemon's resolved runtime configuration (embedder, vuln_source, etc).",
  "properties": {}
}`)

var findSymbolInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "symbol":  {"type": "string", "description": "Symbol name (e.g. \"Promoter.Promote\")."},
    "repo_id": {"type": "string", "description": "Full repo_id or short_id; required when more than one repo is registered."},
    "branch":  {"type": "string", "description": "Branch to search (default: active branch)."},
    "kind":    {"type": "string", "description": "Filter by node kind: function|method|struct|interface|type|package."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["symbol"]
}`)

var getNodeInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "node_id": {"type": "string", "description": "Content-addressed node_id (SHA-256 hex) returned by eng_find_symbol etc."},
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"}
  },
  "required": ["node_id"]
}`)

var getFileNodesInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Returns every node for a single file. The handler accepts 'path' as an alias for 'file_path'.",
  "properties": {
    "file_path": {"type": "string", "description": "Repo-relative or absolute path to the source file."},
    "path":      {"type": "string", "description": "Alias for file_path."},
    "repo_id":   {"type": "string"},
    "branch":    {"type": "string"},
    "cwd":       {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var getCallChainInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Returns the CALLS-edge chain from a node. One of node_id or symbol is required.",
  "properties": {
    "node_id":           {"type": "string", "description": "Resolve directly by node_id."},
    "symbol":            {"type": "string", "description": "Symbol name; resolved against repo_id+branch."},
    "repo_id":           {"type": "string"},
    "branch":            {"type": "string"},
    "depth":             {"type": "integer", "minimum": 1, "maximum": 10, "description": "Traversal depth (default 3, max 10)."},
    "direction":         {"type": "string", "enum": ["in", "out", "both"], "description": "'out' (callees, default), 'in' (callers), or 'both'."},
    "expand_cross_repo": {"type": "boolean", "description": "Follow CALLS edges into other registered repos when true."},
    "cwd":               {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var blastRadiusInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "node_id":           {"type": "string", "description": "node_id to fan out from. Use eng_find_symbol to obtain one. Mutually exclusive with symbol."},
    "symbol":            {"type": "string", "description": "Symbol name to fan out from (parity with eng_get_call_chain). Ambiguous matches are rejected; pass node_id to disambiguate."},
    "repo_id":           {"type": "string"},
    "branch":            {"type": "string"},
    "max_depth":         {"type": "integer", "minimum": 1},
    "max_nodes":         {"type": "integer", "minimum": 1},
    "direction":         {"type": "string", "enum": ["in", "out", "both"]},
    "expand_cross_repo": {"type": "boolean"},
    "cwd":               {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var diffBlastRadiusInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Blast radius of the HEAD commit's diff (computed against HEAD^).",
  "properties": {
    "repo_id":           {"type": "string"},
    "branch":            {"type": "string"},
    "max_depth":         {"type": "integer", "minimum": 1},
    "max_nodes":         {"type": "integer", "minimum": 1},
    "direction":         {"type": "string", "enum": ["in", "out", "both"]},
    "expand_cross_repo": {"type": "boolean"},
    "cwd":               {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var dirtyBlastRadiusInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Blast radius of currently-staged (uncommitted) changes.",
  "properties": {
    "repo_id":           {"type": "string"},
    "branch":            {"type": "string"},
    "max_depth":         {"type": "integer", "minimum": 1},
    "max_nodes":         {"type": "integer", "minimum": 1},
    "direction":         {"type": "string", "enum": ["in", "out", "both"]},
    "expand_cross_repo": {"type": "boolean"},
    "cwd":               {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var contextPackInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Bundles an anchor (one of node_id, symbol, or task_id) with its callers, callees, and tests for LLM prompting. Exactly one of node_id, symbol, or task_id is required.",
  "properties": {
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "node_id": {"type": "string", "description": "Node to anchor on. Mutually exclusive with symbol and task_id."},
    "symbol":  {"type": "string", "description": "Symbol to anchor on. Mutually exclusive with node_id and task_id."},
    "task_id": {"type": "string", "description": "Task to derive the anchor symbol from. Mutually exclusive with node_id and symbol."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var searchSemanticInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Hybrid semantic + lexical search over the indexed graph. Returns RRF-fused results.",
  "properties": {
    "query":   {"type": "string", "description": "Free-text query."},
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "k":       {"type": "integer", "minimum": 1, "description": "Result count (default 10). 'limit' is accepted as an alias; k wins on conflict."},
    "limit":   {"type": "integer", "minimum": 1, "description": "Alias for k."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["query"]
}`)

var searchSimilarInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "k-nearest-neighbour vector search seeded by an existing node. Accepts node_id (exact) or symbol (resolved via FindNodes; ambiguous matches rejected)",
  "properties": {
    "node_id": {"type": "string"},
    "symbol":  {"type": "string", "description": "Alias for node_id by symbol name (resolved like eng_find_symbol). Ambiguity is rejected."},
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "k":       {"type": "integer", "minimum": 1, "description": "Neighbour count (default 10). 'limit' is accepted as an alias."},
    "limit":   {"type": "integer", "minimum": 1, "description": "Alias for k."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var findOwnerInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Returns the owner of a file via CODEOWNERS (longest-match) or git blame fallback. Accepts file_path/path, OR symbol/node_id which resolves to the symbol's defining file.",
  "properties": {
    "file_path": {"type": "string", "description": "Repo-relative path to the file whose dominant committer should be returned."},
    "path":      {"type": "string", "description": "Alias for file_path."},
    "symbol":    {"type": "string", "description": "Symbol name; resolved to the defining file's path."},
    "node_id":   {"type": "string", "description": "Node id; resolved to its file path."},
    "repo_id":   {"type": "string"}
  },
  "required": ["repo_id"]
}`)

var findChangedSymbolsInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "Diff two git refs and return the added/removed/modified symbols. ref_a/ref_b default to HEAD~1/HEAD when both omitted. base/head are accepted as aliases.",
  "properties": {
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "ref_a":   {"type": "string", "description": "Base git ref (e.g. 'main', 'HEAD~5', a SHA). Default HEAD~1."},
    "ref_b":   {"type": "string", "description": "Target git ref. Default HEAD."},
    "base":    {"type": "string", "description": "Alias for ref_a using git's canonical name."},
    "head":    {"type": "string", "description": "Alias for ref_b using git's canonical name."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var findTodosInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id":        {"type": "string"},
    "branch":         {"type": "string"},
    "include_closed": {"type": "boolean", "description": "Include closed TODO findings in the result (default false)."},
    "cwd":            {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var listDependenciesInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "description": "List external modules the repo imports, ranked by call-site usage count (derived from cross_repo_edge_stubs). Each entry includes a small sample of top call sites so an agent can jump straight into the using code.",
  "properties": {
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var listFindingsInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id":            {"type": "string"},
    "branch":             {"type": "string"},
    "state":              {"type": "string", "enum": ["open", "closed"], "description": "Filter by state (default open)."},
    "severity":           {"type": "string", "enum": ["critical", "high", "medium", "low", "info"]},
    "rule":               {"type": "string", "description": "Rule name (e.g. 'vulnerable_dependency', 'dead-code', 'secret_leak')."},
    "include_suppressed": {"type": "boolean", "description": "Surface findings hidden by an active suppression (default false)."},
    "cwd":                {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  }
}`)

var getFindingInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "finding_id": {"type": "string"},
    "branch":     {"type": "string", "description": "Optional; finding_id is globally unique."},
    "repo_id":    {"type": "string", "description": "Optional; informational only (finding_id is globally unique)."}
  },
  "required": ["finding_id"]
}`)

var reopenFindingInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "finding_id": {"type": "string"},
    "branch":     {"type": "string"},
    "repo_id":    {"type": "string"}
  },
  "required": ["finding_id"]
}`)

var listSuppressionsInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"}
  }
}`)

var getSuppressionInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "suppression_id": {"type": "string"}
  },
  "required": ["suppression_id"]
}`)

var closeSuppressionInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "suppression_id": {"type": "string"},
    "repo_id":        {"type": "string", "description": "Optional, audit attribution only."}
  },
  "required": ["suppression_id"]
}`)

var hotZoneInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id": {"type": "string"},
    "branch":  {"type": "string"},
    "limit":   {"type": "integer", "minimum": 1, "description": "Max files to return (0 = service default; large values capped)."},
    "cwd":     {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["repo_id"]
}`)

var entryPointsInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "repo_id":       {"type": "string"},
    "branch":        {"type": "string"},
    "include_tests": {"type": "boolean", "description": "Include Test*/Benchmark*/Example*/Fuzz* and *_test.go entries (default false)."},
    "limit":         {"type": "integer", "minimum": 1},
    "cwd":           {"type": "string", "description": "Working directory used to resolve the active repo when repo_id is omitted."}
  },
  "required": ["repo_id"]
}`)
