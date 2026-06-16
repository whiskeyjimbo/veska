package initcmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// AgentSnippetSentinel is the marker string both written into and
// scanned for to make `veska init --agent X` idempotent. Re-running
// the command finds the sentinel in the target file and skips the
// append. Choosing an HTML comment keeps it invisible in rendered
// Markdown but trivially greppable.
const AgentSnippetSentinel = "<!-- veska:init -->"

// agentSnippetBody is the per-agent instruction block.
// Lists the four MCP tools an agent reaches for most often plus a
// one-line "when to use" so the agent doesn't have to guess. Kept
// terse on purpose — these files are loaded into every conversation
// and long blocks consume context the agent could spend on the task.
const agentSnippetBody = AgentSnippetSentinel + `
## Veska code-graph tools

This repo is indexed by Veska. Prefer these MCP tools over re-grepping the
tree — they reason from the parsed graph, not raw text.

**` + "`repo_id`" + ` and ` + "`branch`" + ` are usually optional.** The daemon resolves
` + "`repo_id`" + ` from cwd (preferred — works in multi-repo setups) or, as a
fallback, from the single registered repo. ` + "`branch`" + ` defaults to the
repo's active branch. Pass them explicitly only when operating outside the
cwd repo or against a non-current branch. Discovery helpers:
` + "`eng_get_current_repo`" + ` (cwd → repo) and ` + "`eng_list_repos`" + ` (full set).

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
// mcpConfigPath, when non-empty, points at a JSON file the harness
// reads to discover MCP servers (`.mcp.json` for Claude Code project
// scope, `.cursor/mcp.json` for Cursor). On `veska init --agent X`,
// veska merges itself into that file's mcpServers map — idempotent,
// preserves other servers.
type agentFlavor struct {
	name          string
	path          string
	mcpConfigPath string // empty when the harness doesn't speak MCP via a JSON config
}

// agentFlavors are the harnesses promises to support. The
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

// SupportedFlavorNames returns the sorted list of flavor names — used
// in the unknown-flavor error message and the --agent flag help text.
func SupportedFlavorNames() []string {
	names := make([]string, 0, len(agentFlavors))
	for _, f := range agentFlavors {
		names = append(names, f.name)
	}
	sort.Strings(names)
	return names
}

// AgentSnippetParams bundles the inputs for WriteAgentSnippet. It exists to
// keep the call within the project's argument budget and to carry the
// preview/confirm controls (AssumeYes, Interactive, In) added in
// so init --agent never silently mutates the wrong tree.
type AgentSnippetParams struct {
	RootDir         string    // project root the snippet is written under
	Flavor          string    // agent flavor (claude, cursor, …)
	Out             io.Writer // status + preview sink
	In              io.Reader // stdin for the interactive confirm prompt
	UpdateGitignore bool      // also manage a veska block in.gitignore
	AssumeYes       bool      // skip the prompt and write (-y / --yes)
	Interactive     bool      // stdin is a TTY → prompt instead of bailing
}

// WriteAgentSnippet writes (or appends) the per-flavor snippet under
// p.RootDir and reports the action taken to p.Out. Before writing it prints a
// preview of every file it will touch (with the absolute root path) and gates
// the write on confirmation: AssumeYes writes immediately; an interactive TTY
// prompts [Y/n]; a non-interactive stdin without AssumeYes prints the preview
// and writes NOTHING ( — prevents accidental writes in the wrong
// tree / in automation). Idempotent: a second call against the same
// rootDir+flavor detects the sentinel and reports "already present".
func WriteAgentSnippet(p AgentSnippetParams) error {
	f, ok := lookupFlavor(p.Flavor)
	if !ok {
		return fmt.Errorf("unknown agent flavor %q (supported: %s)",
			p.Flavor, strings.Join(SupportedFlavorNames(), ", "))
	}

	rootDir, out, updateGitignore := p.RootDir, p.Out, p.UpdateGitignore

	plan := buildAgentPlan(f, rootDir, updateGitignore)
	printAgentPlan(out, rootDir, plan)
	proceed, err := confirmAgentWrite(p, out)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	return applyAgentSnippet(f, p)
}

// applyAgentSnippet performs the confirmed writes: the instruction file, the
// MCP-server config (when the flavor speaks MCP), and the opt-in.gitignore
// block. Split from WriteAgentSnippet so the preview/confirm orchestration and
// the write body each stay within the size budget.
func applyAgentSnippet(f agentFlavor, p AgentSnippetParams) error {
	rootDir, out, updateGitignore := p.RootDir, p.Out, p.UpdateGitignore

	target := filepath.Join(rootDir, f.path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", target, err)
	}

	existing, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", target, err)
	}

	instructionsAlreadyPresent := strings.Contains(string(existing), AgentSnippetSentinel)
	if instructionsAlreadyPresent {
		fmt.Fprintf(out, "veska: %s already present at %s\n", p.Flavor, target)
	} else {
		body := buildAppendBody(existing, agentSnippetBody)
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		if len(existing) == 0 {
			fmt.Fprintf(out, "veska: wrote %s instructions to %s\n", p.Flavor, target)
		} else {
			fmt.Fprintf(out, "veska: appended %s instructions to %s\n", p.Flavor, target)
		}
	}

	// when the harness reads an MCP-server config JSON,
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
			if action, err := EnsureMcpServerEntry(cfgPath, "veska", mcpBin); err != nil {
				fmt.Fprintf(out, "veska: warning: could not update %s: %v\n", cfgPath, err)
			} else {
				fmt.Fprintf(out, "veska: %s %s in %s\n", action, "veska MCP server", cfgPath)
			}
		}
	}

	// Opt-in.gitignore management: silently modifying a tracked
	// file surprises users who only asked for the instruction file. Pass
	// update-gitignore to opt in. The block is still bracketed by sentinels
	// so a re-run leaves an already-managed block alone.
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

// agentPlanItem is one previewed file write: the absolute path and the verb
// describing what will happen to it (create / append / register / …). The
// verb is computed read-only so the preview matches what the writer does.
type agentPlanItem struct {
	path   string
	action string
}

// buildAgentPlan computes — without writing anything — the set of files
// WriteAgentSnippet will touch and the action for each, so the preview is
// accurate. Mirrors the writer's decision logic (sentinel scan for the
// instruction file, mcpServerAction for the MCP config).
func buildAgentPlan(f agentFlavor, rootDir string, updateGitignore bool) []agentPlanItem {
	plan := make([]agentPlanItem, 0, 3)

	target := filepath.Join(rootDir, f.path)
	existing, _ := os.ReadFile(target)
	switch {
	case strings.Contains(string(existing), AgentSnippetSentinel):
		plan = append(plan, agentPlanItem{target, "already present"})
	case len(existing) == 0:
		plan = append(plan, agentPlanItem{target, "create veska block"})
	default:
		plan = append(plan, agentPlanItem{target, "append veska block"})
	}

	if f.mcpConfigPath != "" {
		cfgPath := filepath.Join(rootDir, f.mcpConfigPath)
		mcpBin, err := resolveVeskaMcpPath()
		if err != nil {
			plan = append(plan, agentPlanItem{cfgPath, "register veska MCP server (path TBD)"})
		} else {
			plan = append(plan, agentPlanItem{cfgPath, mcpServerAction(cfgPath, "veska", mcpBin)})
		}
	}

	if updateGitignore {
		plan = append(plan, agentPlanItem{filepath.Join(rootDir, ".gitignore"), "manage veska block"})
	}
	return plan
}

// printAgentPlan emits the preview. The absolute root is shown on its own
// line so a user who launched init in the wrong directory notices before
// confirming.
func printAgentPlan(out io.Writer, rootDir string, plan []agentPlanItem) {
	fmt.Fprintf(out, "veska init --agent will write under: %s\n", rootDir)
	for _, item := range plan {
		fmt.Fprintf(out, "  %s (%s)\n", item.path, item.action)
	}
}

// confirmAgentWrite resolves whether to proceed with the write. AssumeYes →
// yes without prompting. Non-interactive stdin without AssumeYes → no (prints
// the -y hint; deliberately safer than defaulting to accept). Interactive →
// prompt [Y/n], Enter/y = yes, n = no. Mirrors the ResolveVulnChoice idiom.
func confirmAgentWrite(p AgentSnippetParams, out io.Writer) (bool, error) {
	if p.AssumeYes {
		return true, nil
	}
	if !p.Interactive || p.In == nil {
		fmt.Fprintln(out, "veska: re-run with -y to apply (non-interactive stdin); nothing written")
		return false, nil
	}
	reader := bufio.NewReader(p.In)
	fmt.Fprint(out, "Apply these changes? [Y/n] ")
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF before any answer — treat as the safe non-interactive default.
		fmt.Fprintln(out, "veska: no input; nothing written (re-run with -y to apply)")
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	default:
		fmt.Fprintln(out, "veska: declined; nothing written")
		return false, nil
	}
}

// mcpServerAction reports the verb EnsureMcpServerEntry would use for cfgPath
// WITHOUT writing — read-only counterpart used by the preview so it can't
// drift from the actual write. On any read/parse trouble it optimistically
// reports "register", matching the create path.
func mcpServerAction(cfgPath, name, command string) string {
	existing, err := os.ReadFile(cfgPath)
	if err != nil || len(existing) == 0 {
		return "register veska MCP server"
	}
	var cfg map[string]any
	if json.Unmarshal(existing, &cfg) != nil {
		return "register veska MCP server"
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	prior, ok := servers[name].(map[string]any)
	if !ok {
		return "register veska MCP server"
	}
	if prior["command"] == command {
		return "already registered"
	}
	return "update veska MCP server"
}

// resolveVeskaMcpPath returns the absolute path to the veska-mcp binary
// that the running veska invocation should advertise to MCP harnesses.
// The lookup goes: PATH (the binary is installed), then the running
// binary's directory (veska-mcp sits next to veska in dev clones and in
// the release tarball).
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

// EnsureMcpServerEntry merges {name: {command, args:}} into cfgPath's
// mcpServers map. Creates cfgPath when absent; preserves unrelated keys
// and other server entries when present. Returns the verb used for the
// status line ("registered", "updated", "already registered"). Errors
// on JSON parse failure — corruption is surprising and we'd rather the
// user fix it than silently overwrite their file.
func EnsureMcpServerEntry(cfgPath, name, command string) (string, error) {
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
// block in a repo's.gitignore. Re-running `veska init --agent` finds the
// block, leaves user-added lines outside it alone, and rewrites only what
// lives between the sentinels.
const (
	gitignoreSentinelBegin = "# >>> veska-managed (do not edit between these markers) >>>"
	gitignoreSentinelEnd   = "# <<< veska-managed <<<"
)

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
