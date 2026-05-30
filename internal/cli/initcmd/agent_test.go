package initcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteAgentSnippet_KnownFlavorsCreateExpectedPath: every flavor
// listed in the issue (claude, cursor, codex, opencode, copilot,
// gemini, kiro) must resolve to a defined file path under the project
// root and create the file with the snippet inside.
func TestWriteAgentSnippet_KnownFlavorsCreateExpectedPath(t *testing.T) {
	cases := []struct {
		flavor   string
		wantPath string // relative to project root
	}{
		{"claude", "CLAUDE.md"},
		{"cursor", ".cursor/rules/veska.mdc"},
		{"codex", "AGENTS.md"},
		{"opencode", "AGENTS.md"},
		{"copilot", ".github/copilot-instructions.md"},
		{"gemini", "GEMINI.md"},
		{"kiro", ".kiro/steering/veska.md"},
	}
	for _, c := range cases {
		t.Run(c.flavor, func(t *testing.T) {
			root := t.TempDir()
			var buf bytes.Buffer
			if err := WriteAgentSnippet(root, c.flavor, &buf, true); err != nil {
				t.Fatalf("WriteAgentSnippet(%q): %v", c.flavor, err)
			}
			abs := filepath.Join(root, c.wantPath)
			body, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("expected file %s: %v", abs, err)
			}
			content := string(body)
			// Sanity: snippet must mention all four required tools.
			for _, tool := range []string{
				"eng_search_semantic",
				"eng_find_symbol",
				"eng_get_call_chain",
				"eng_get_context_pack",
			} {
				if !strings.Contains(content, tool) {
					t.Errorf("snippet for %s missing %s", c.flavor, tool)
				}
			}
		})
	}
}

// TestWriteAgentSnippet_Idempotent: a second invocation against the
// same root must be a no-op — the file content must NOT contain the
// sentinel twice, and the report must say "already present".
func TestWriteAgentSnippet_Idempotent(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer

	if err := WriteAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))

	buf.Reset()
	if err := WriteAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))

	if string(first) != string(second) {
		t.Errorf("second call modified the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if got := strings.Count(string(second), AgentSnippetSentinel); got != 1 {
		t.Errorf("sentinel count after re-run: got %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "already present") {
		t.Errorf("second-call report should say 'already present', got: %s", buf.String())
	}
}

// TestWriteAgentSnippet_AppendsToExistingFile: when CLAUDE.md (or
// AGENTS.md, etc.) exists with the user's own content, the snippet is
// appended without losing the existing content.
func TestWriteAgentSnippet_AppendsToExistingFile(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "CLAUDE.md")
	preface := "# my project rules\n\nbe careful with X.\n"
	if err := os.WriteFile(existing, []byte(preface), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("writeAgentSnippet: %v", err)
	}
	body, _ := os.ReadFile(existing)
	if !strings.HasPrefix(string(body), preface) {
		t.Errorf("existing content lost; file=%q", body)
	}
	if !strings.Contains(string(body), AgentSnippetSentinel) {
		t.Errorf("snippet not appended")
	}
}

// TestWriteAgentSnippet_GitignoreOptIn guards solov2-zm6i: the default call
// (updateGitignore=false) must NOT create or modify .gitignore; only the
// explicit opt-in path writes the veska-managed block.
func TestWriteAgentSnippet_GitignoreOptIn(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	if err := WriteAgentSnippet(root, "claude", &buf, false); err != nil {
		t.Fatalf("writeAgentSnippet: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("expected no .gitignore created on default path, got err=%v", err)
	}

	root2 := t.TempDir()
	buf.Reset()
	if err := WriteAgentSnippet(root2, "claude", &buf, true); err != nil {
		t.Fatalf("writeAgentSnippet (opt-in): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root2, ".gitignore")); err != nil {
		t.Errorf("expected .gitignore created with --update-gitignore, got %v", err)
	}
}

// TestEnsureMcpServerEntry_CreatesFile covers solov2-zo0w: writing
// veska into a missing .mcp.json creates the file with the right
// shape and returns "registered".
func TestEnsureMcpServerEntry_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".mcp.json")
	verb, err := EnsureMcpServerEntry(cfgPath, "veska", "/usr/local/bin/veska-mcp")
	if err != nil {
		t.Fatalf("ensureMcpServerEntry: %v", err)
	}
	if verb != "registered" {
		t.Errorf("verb = %q, want registered", verb)
	}
	b, _ := os.ReadFile(cfgPath)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong shape: %v", got)
	}
	veskaEntry, ok := servers["veska"].(map[string]any)
	if !ok {
		t.Fatalf("veska entry missing: %v", servers)
	}
	if veskaEntry["command"] != "/usr/local/bin/veska-mcp" {
		t.Errorf("command = %v, want /usr/local/bin/veska-mcp", veskaEntry["command"])
	}
}

// TestEnsureMcpServerEntry_PreservesOtherServers guards the "don't
// stomp on other MCP servers" invariant — a project that already
// registered, say, github + linear must keep both after veska's
// merge.
func TestEnsureMcpServerEntry_PreservesOtherServers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".mcp.json")
	prior := `{
  "mcpServers": {
    "github": {"command": "/usr/bin/gh-mcp", "args": []},
    "linear": {"command": "/usr/bin/linear-mcp", "args": []}
  },
  "otherToplevelKey": "preserved"
}`
	if err := os.WriteFile(cfgPath, []byte(prior), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := EnsureMcpServerEntry(cfgPath, "veska", "/bin/veska-mcp"); err != nil {
		t.Fatalf("ensureMcpServerEntry: %v", err)
	}
	b, _ := os.ReadFile(cfgPath)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	servers := got["mcpServers"].(map[string]any)
	for _, name := range []string{"github", "linear", "veska"} {
		if _, ok := servers[name]; !ok {
			t.Errorf("server %q missing after merge: %v", name, servers)
		}
	}
	if got["otherToplevelKey"] != "preserved" {
		t.Errorf("top-level key lost: %v", got)
	}
}

// TestEnsureMcpServerEntry_IdempotentSameCommand: a second call with
// the same command must report "already registered" and not bump the
// file's contents (no spurious diffs in version control).
func TestEnsureMcpServerEntry_IdempotentSameCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".mcp.json")
	const cmd = "/bin/veska-mcp"
	if _, err := EnsureMcpServerEntry(cfgPath, "veska", cmd); err != nil {
		t.Fatalf("first: %v", err)
	}
	verb, err := EnsureMcpServerEntry(cfgPath, "veska", cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if verb != "already registered" {
		t.Errorf("verb = %q, want already registered", verb)
	}
}

// TestEnsureMcpServerEntry_UpdatesChangedCommand: when the user moves
// the veska-mcp binary (e.g. reinstalled to /usr/local/bin), the
// re-run should update the entry rather than silently leaving the
// stale path.
func TestEnsureMcpServerEntry_UpdatesChangedCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".mcp.json")
	if _, err := EnsureMcpServerEntry(cfgPath, "veska", "/old/path/veska-mcp"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	verb, err := EnsureMcpServerEntry(cfgPath, "veska", "/new/path/veska-mcp")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if verb != "updated" {
		t.Errorf("verb = %q, want updated", verb)
	}
	b, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	cmd := got["mcpServers"].(map[string]any)["veska"].(map[string]any)["command"]
	if cmd != "/new/path/veska-mcp" {
		t.Errorf("command = %v, want /new/path/veska-mcp", cmd)
	}
}

// TestWriteAgentSnippet_UnknownFlavorErrors: an unknown flavor must
// surface a helpful error listing the supported flavors — otherwise
// the user has no way to discover what they typed wrong.
func TestWriteAgentSnippet_UnknownFlavorErrors(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	err := WriteAgentSnippet(root, "vim-fugitive", &buf, true)
	if err == nil {
		t.Fatal("expected error for unknown flavor")
	}
	for _, want := range []string{"claude", "cursor", "kiro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q for discoverability: %v", want, err)
		}
	}
}
