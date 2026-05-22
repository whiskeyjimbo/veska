package main

import (
	"fmt"
	"io"
	"os"
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

- ` + "`eng_search_semantic`" + ` — natural-language → ranked code chunks. Use when
  the user describes behavior ("where do we validate session tokens"). Inline
  snippets are returned; you usually don't need a follow-up Read.
  Example: ` + "`{query: \"parse config\", repo_id: \"<id>\", branch: \"main\"}`" + `

- ` + "`eng_find_symbol`" + ` — exact symbol lookup by name or symbol_path. Use
  when you know the identifier ("show me ParseConfig").

- ` + "`eng_get_call_chain`" + ` — incoming/outgoing CALLS edges for a node. Use
  to answer "what calls this" or "what does this reach" without manually
  tracing through files.

- ` + "`eng_get_context_pack`" + ` — bundles a seed node with its callers,
  callees, and tests into a single payload. Use at the start of a non-trivial
  change so you don't have to assemble the surrounding context piecewise.

If a search returns no hits, the index may be stale or the repo may not be
registered. Run ` + "`veska doctor status`" + ` to check.
<!-- /veska:init -->
`

// agentFlavor describes a known AI-agent harness: the canonical name
// the user passes via --agent, and the relative file path under the
// project root where its instruction snippet lives.
type agentFlavor struct {
	name string
	path string
}

// agentFlavors are the harnesses solov2-m81 promises to support. The
// path conventions follow each tool's documented instruction-file
// location at the time of writing; harness vendors may move these,
// in which case we update the table.
var agentFlavors = []agentFlavor{
	{name: "claude", path: "CLAUDE.md"},
	{name: "codex", path: "AGENTS.md"},
	{name: "opencode", path: "AGENTS.md"},
	{name: "cursor", path: ".cursor/rules/veska.mdc"},
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
func writeAgentSnippet(rootDir, flavor string, out io.Writer) error {
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

	if strings.Contains(string(existing), agentSnippetSentinel) {
		fmt.Fprintf(out, "veska: %s already present at %s\n", flavor, target)
		return nil
	}

	body := buildAppendBody(existing, agentSnippetBody)
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if len(existing) == 0 {
		fmt.Fprintf(out, "veska: wrote %s instructions to %s\n", flavor, target)
	} else {
		fmt.Fprintf(out, "veska: appended %s instructions to %s\n", flavor, target)
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
