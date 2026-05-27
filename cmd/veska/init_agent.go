package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// agentSnippetSentinel is the marker string both written into and
// scanned for to make `veska init --agent X` idempotent. Re-running
// the command finds the sentinel in the target file and skips the
// append. Choosing an HTML comment keeps it invisible in rendered
// Markdown but trivially greppable.
const agentSnippetSentinel = "<!-- veska:init -->"

// agentSnippetBody is the per-agent instruction block (solov2-m81).
// Lists the four MCP tools an agent reaches for most often plus a
// one-line "when to use" so the agent doesn't have to guess. Kept
// terse on purpose — these files are loaded into every conversation
// and long blocks consume context the agent could spend on the task.
const agentSnippetBody = agentSnippetSentinel + `
## Veska code-graph tools

This repo is indexed by Veska. Prefer these MCP tools over re-grepping the
tree — they reason from the parsed graph, not raw text.

**` + "`repo_id`" + ` and ` + "`branch`" + ` are usually optional.** When exactly one repo
is registered the daemon auto-resolves ` + "`repo_id`" + `; when ` + "`branch`" + ` is omitted
it defaults to the repo's active branch. Pass them explicitly only when
operating across multiple repos or against a non-current branch. To list the
known repos: ` + "`eng_list_repos`" + `; to discover the current one from cwd:
` + "`eng_get_current_repo`" + `.

- ` + "`eng_search_semantic`" + ` — natural-language → ranked code chunks. Use when
  the user describes behavior ("where do we validate session tokens"). Inline
  snippets are returned; you usually don't need a follow-up Read.
  Example: ` + "`{query: \"parse config\"}`" + ` (or pass ` + "`repo_id`" + `/` + "`branch`" + ` explicitly).

- ` + "`eng_find_symbol`" + ` — exact symbol lookup by name or symbol_path. Use
  when you know the identifier ("show me ParseConfig").

- ` + "`eng_get_call_chain`" + ` — CALLS-edge BFS for a node. Accepts either
  ` + "`node_id`" + ` or ` + "`symbol`" + ` (parity with ` + "`eng_find_symbol`" + `), and
  ` + "`direction`" + ` = ` + "`out`" + ` (default, callees) / ` + "`in`" + ` (callers) /
  ` + "`both`" + `. Use to answer "what does this reach" (` + "`out`" + `) or "what calls
  this" (` + "`in`" + `) without manually tracing through files.

- ` + "`eng_get_context_pack`" + ` — bundles a seed node with its callers,
  callees, and tests into a single payload. Use at the start of a non-trivial
  change so you don't have to assemble the surrounding context piecewise.

**Other tools available** — call ` + "`tools/list`" + ` for full schemas; reach
for these when the four above aren't enough:

- ` + "`eng_get_node`" + `, ` + "`eng_get_file_nodes`" + ` — node-by-id /
  per-file listing when you already have an identifier.
- ` + "`eng_find_changed_symbols`" + `, ` + "`eng_get_diff_blast_radius`" + ` — symbol-grain
  diff and downstream-impact between two git refs, for PR review and regression triage.
- ` + "`eng_list_repos`" + `, ` + "`eng_get_repo`" + `, ` + "`eng_get_current_repo`" + ` — repo registry inspection.
- ` + "`eng_list_dependencies`" + ` — modules this repo calls into, with sampled call-sites.
- ` + "`eng_list_findings`" + `, ` + "`eng_get_finding`" + `, ` + "`eng_close_finding`" + `, ` + "`eng_suppress_finding`" + `,
  ` + "`eng_list_suppressions`" + `, ` + "`eng_close_suppression`" + ` — promotion-check findings:
  vuln deps, secret leaks, dead code, auto-link candidates. Use ` + "`eng_close_finding`" + `
  to record an "accept" / "fixed" decision an agent can defend later.
- ` + "`eng_add_repo`" + ` — register a new repo path; the daemon kicks off a cold scan.
- ` + "`eng_get_status`" + ` — daemon health + in-flight scans + pending-embed counts.

If a search returns no hits, the index may be stale or the repo may not be
registered. Run ` + "`veska doctor status`" + ` to check.

**Cold-start handling.** When ` + "`eng_search_semantic`" + ` returns
` + "`embeddings_pending`" + ` in ` + "`degraded_reasons`" + `, the daemon is still
embedding nodes — results are partial. Call ` + "`eng_get_status`" + ` and inspect
` + "`scans_in_flight[]`" + `: ` + "`phase=walking`" + ` or ` + "`phase=promoting`" + ` means
"wait a bit and retry"; an empty ` + "`scans_in_flight`" + ` plus a non-zero
` + "`pending_embeds`" + ` means the embedder worker is draining the queue (also
worth a retry). No in-flight scan and ` + "`pending_embeds=0`" + ` means the
query genuinely matched nothing — don't loop.
<!-- /veska:init -->
`

// agentFlavor describes a known AI-agent harness: the canonical name
// the user passes via --agent, and the relative file path under the
// project root where its instruction snippet lives.
//
// mcpConfigPath, when non-empty, points at a JSON file the harness
// reads to discover MCP servers (`.mcp.json` for Claude Code project
// scope, `.cursor/mcp.json` for Cursor). On `veska init --agent X`,
// veska merges itself into that file's mcpServers map — idempotent,
// preserves other servers (solov2-zo0w).
type agentFlavor struct {
	name          string
	path          string
	mcpConfigPath string // empty when the harness doesn't speak MCP via a JSON config
}

// agentFlavors are the harnesses solov2-m81 promises to support. The
// path conventions follow each tool's documented instruction-file
// location at the time of writing; harness vendors may move these,
// in which case we update the table.
var agentFlavors = []agentFlavor{
	{name: "claude", path: "CLAUDE.md", mcpConfigPath: ".mcp.json"},
	{name: "codex", path: "AGENTS.md"},
	{name: "opencode", path: "AGENTS.md"},
	{name: "cursor", path: ".cursor/rules/veska.mdc", mcpConfigPath: ".cursor/mcp.json"},
	{name: "copilot", path: ".github/copilot-instructions.md"},
	{name: "gemini", path: "GEMINI.md"},
	{name: "kiro", path: ".kiro/steering/veska.md"},
}

func lookupFlavor(name string) (agentFlavor, bool) {
	for _, f := range agentFlavors {
		if f.name == name {
			return f, true
		}
	}
	return agentFlavor{}, false
}

// supportedFlavorNames returns the sorted list of flavor names — used
// in the unknown-flavor error message and the --agent flag help text.
func supportedFlavorNames() []string {
	names := make([]string, 0, len(agentFlavors))
	for _, f := range agentFlavors {
		names = append(names, f.name)
	}
	sort.Strings(names)
	return names
}

// writeAgentSnippet writes (or appends) the per-flavor snippet under
// rootDir and reports the action taken to out. Idempotent: a second
// call against the same rootDir+flavor detects the sentinel already
// present and reports "already present" without modifying the file.
func writeAgentSnippet(rootDir, flavor string, out io.Writer, updateGitignore bool) error {
	f, ok := lookupFlavor(flavor)
	if !ok {
		return fmt.Errorf("unknown agent flavor %q (supported: %s)",
			flavor, strings.Join(supportedFlavorNames(), ", "))
	}

	target := filepath.Join(rootDir, f.path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", target, err)
	}

	existing, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", target, err)
	}

	instructionsAlreadyPresent := strings.Contains(string(existing), agentSnippetSentinel)
	if instructionsAlreadyPresent {
		fmt.Fprintf(out, "veska: %s already present at %s\n", flavor, target)
	} else {
		body := buildAppendBody(existing, agentSnippetBody)
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		if len(existing) == 0 {
			fmt.Fprintf(out, "veska: wrote %s instructions to %s\n", flavor, target)
		} else {
			fmt.Fprintf(out, "veska: appended %s instructions to %s\n", flavor, target)
		}
	}

	// solov2-zo0w: when the harness reads an MCP-server config JSON,
	// merge veska in so the user doesn't have to hand-edit. Idempotent:
	// preserves other servers; skips when veska is already registered
	// with the same command. Non-fatal on error — the instruction file
	// (the primary deliverable) is already written.
	if f.mcpConfigPath != "" {
		mcpBin, err := resolveVeskaMcpPath()
		if err != nil {
			fmt.Fprintf(out, "veska: warning: could not resolve veska-mcp path for MCP config: %v\n", err)
		} else {
			cfgPath := filepath.Join(rootDir, f.mcpConfigPath)
			if action, err := ensureMcpServerEntry(cfgPath, "veska", mcpBin); err != nil {
				fmt.Fprintf(out, "veska: warning: could not update %s: %v\n", cfgPath, err)
			} else {
				fmt.Fprintf(out, "veska: %s %s in %s\n", action, "veska MCP server", cfgPath)
			}
		}
	}

	// Opt-in .gitignore management (solov2-zm6i): silently modifying a tracked
	// file surprises users who only asked for the instruction file. Pass
	// --update-gitignore to opt in. The block is still bracketed by sentinels
	// so a re-run leaves an already-managed block alone (solov2-t8re).
	if updateGitignore {
		if err := ensureGitignoreStanza(rootDir, out); err != nil {
			// Non-fatal: the snippet is what the user asked for; gitignore
			// upkeep is opportunistic.
			fmt.Fprintf(out, "veska: warning: could not update .gitignore: %v\n", err)
		}
	} else {
		fmt.Fprintln(out, "veska: tip: pass --update-gitignore to also add a veska-managed .gitignore block (covers generated artifacts under docs/veska/)")
	}
	return nil
}

// resolveVeskaMcpPath returns the absolute path to the veska-mcp binary
// that the running veska invocation should advertise to MCP harnesses.
// The lookup goes: PATH (the binary is installed), then the running
// binary's directory (veska-mcp sits next to veska in dev clones and in
// the release tarball). solov2-zo0w.
func resolveVeskaMcpPath() (string, error) {
	if p, err := exec.LookPath("veska-mcp"); err == nil {
		// Resolve to an absolute path so the config doesn't break when
		// the user's PATH changes (e.g. a different shell session).
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs, nil
		}
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate veska binary: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(exe), "veska-mcp")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("veska-mcp not found on PATH or alongside %s", exe)
}

// ensureMcpServerEntry merges {name: {command, args:[]}} into cfgPath's
// mcpServers map. Creates cfgPath when absent; preserves unrelated keys
// and other server entries when present. Returns the verb used for the
// status line ("registered", "updated", "already registered"). Errors
// on JSON parse failure — corruption is surprising and we'd rather the
// user fix it than silently overwrite their file.
func ensureMcpServerEntry(cfgPath, name, command string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return "", fmt.Errorf("create dir for %s: %w", cfgPath, err)
	}
	var cfg map[string]any
	existing, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read %s: %w", cfgPath, err)
	}
	if len(existing) > 0 {
		if uerr := json.Unmarshal(existing, &cfg); uerr != nil {
			return "", fmt.Errorf("parse %s: %w", cfgPath, uerr)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	serversRaw, ok := cfg["mcpServers"]
	var servers map[string]any
	if ok {
		if m, ismap := serversRaw.(map[string]any); ismap {
			servers = m
		} else {
			return "", fmt.Errorf("%s mcpServers is not a JSON object", cfgPath)
		}
	} else {
		servers = map[string]any{}
		cfg["mcpServers"] = servers
	}

	verb := "registered"
	if prior, ok := servers[name].(map[string]any); ok {
		if prior["command"] == command {
			return "already registered", nil
		}
		verb = "updated"
	}
	servers[name] = map[string]any{
		"command": command,
		"args":    []string{},
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", cfgPath, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", cfgPath, err)
	}
	return verb, nil
}

// gitignoreSentinelBegin / gitignoreSentinelEnd bracket the veska-managed
// block in a repo's .gitignore. Re-running `veska init --agent` finds the
// block, leaves user-added lines outside it alone, and rewrites only what
// lives between the sentinels.
const gitignoreSentinelBegin = "# >>> veska-managed (do not edit between these markers) >>>"
const gitignoreSentinelEnd = "# <<< veska-managed <<<"

// gitignoreStanza is the body veska maintains inside the sentinel block.
// Kept minimal: only paths veska itself writes. Edit this list (and bump the
// sentinel format if you must) rather than appending one-off entries — the
// re-run logic relies on the block being a single atomic unit.
const gitignoreStanza = gitignoreSentinelBegin + `
docs/veska/
` + gitignoreSentinelEnd + "\n"

// ensureGitignoreStanza writes (or refreshes) the veska-managed block in
// <rootDir>/.gitignore. Idempotent: a second call against the same rootDir
// is a no-op when the block content already matches.
func ensureGitignoreStanza(rootDir string, out io.Writer) error {
	target := filepath.Join(rootDir, ".gitignore")
	existing, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", target, err)
	}
	text := string(existing)
	if i := strings.Index(text, gitignoreSentinelBegin); i >= 0 {
		j := strings.Index(text[i:], gitignoreSentinelEnd)
		if j < 0 {
			return fmt.Errorf("%s contains an unterminated veska-managed block", target)
		}
		// Replace the existing block (including the end sentinel and the
		// trailing newline if present).
		end := i + j + len(gitignoreSentinelEnd)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		newText := text[:i] + gitignoreStanza + text[end:]
		if newText == text {
			return nil
		}
		return os.WriteFile(target, []byte(newText), 0o644)
	}
	// No existing block — append (with a separating newline if needed).
	prefix := text
	if len(prefix) > 0 && !strings.HasSuffix(prefix, "\n") {
		prefix += "\n"
	}
	if len(prefix) > 0 && !strings.HasSuffix(prefix, "\n\n") {
		prefix += "\n"
	}
	if err := os.WriteFile(target, []byte(prefix+gitignoreStanza), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if len(existing) == 0 {
		fmt.Fprintf(out, "veska: wrote .gitignore at %s\n", target)
	} else {
		fmt.Fprintf(out, "veska: appended veska-managed block to %s\n", target)
	}
	return nil
}

// buildAppendBody concatenates existing content and the snippet,
// inserting a blank-line separator only when needed so we don't pile
// up trailing whitespace on idempotent re-runs against files we
// previously created.
func buildAppendBody(existing []byte, snippet string) []byte {
	if len(existing) == 0 {
		return []byte(snippet)
	}
	sep := ""
	if !strings.HasSuffix(string(existing), "\n") {
		sep = "\n\n"
	} else if !strings.HasSuffix(string(existing), "\n\n") {
		sep = "\n"
	}
	return []byte(string(existing) + sep + snippet)
}
