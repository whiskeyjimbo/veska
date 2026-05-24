package main

import (
	"bytes"
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
			if err := writeAgentSnippet(root, c.flavor, &buf, true); err != nil {
				t.Fatalf("writeAgentSnippet(%q): %v", c.flavor, err)
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

	if err := writeAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))

	buf.Reset()
	if err := writeAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("second call: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(root, "CLAUDE.md"))

	if string(first) != string(second) {
		t.Errorf("second call modified the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if got := strings.Count(string(second), agentSnippetSentinel); got != 1 {
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
	if err := writeAgentSnippet(root, "claude", &buf, true); err != nil {
		t.Fatalf("writeAgentSnippet: %v", err)
	}
	body, _ := os.ReadFile(existing)
	if !strings.HasPrefix(string(body), preface) {
		t.Errorf("existing content lost; file=%q", body)
	}
	if !strings.Contains(string(body), agentSnippetSentinel) {
		t.Errorf("snippet not appended")
	}
}

// TestWriteAgentSnippet_GitignoreOptIn guards solov2-zm6i: the default call
// (updateGitignore=false) must NOT create or modify .gitignore; only the
// explicit opt-in path writes the veska-managed block.
func TestWriteAgentSnippet_GitignoreOptIn(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	if err := writeAgentSnippet(root, "claude", &buf, false); err != nil {
		t.Fatalf("writeAgentSnippet: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("expected no .gitignore created on default path, got err=%v", err)
	}

	root2 := t.TempDir()
	buf.Reset()
	if err := writeAgentSnippet(root2, "claude", &buf, true); err != nil {
		t.Fatalf("writeAgentSnippet (opt-in): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root2, ".gitignore")); err != nil {
		t.Errorf("expected .gitignore created with --update-gitignore, got %v", err)
	}
}

// TestWriteAgentSnippet_UnknownFlavorErrors: an unknown flavor must
// surface a helpful error listing the supported flavors — otherwise
// the user has no way to discover what they typed wrong.
func TestWriteAgentSnippet_UnknownFlavorErrors(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	err := writeAgentSnippet(root, "vim-fugitive", &buf, true)
	if err == nil {
		t.Fatal("expected error for unknown flavor")
	}
	for _, want := range []string{"claude", "cursor", "kiro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q for discoverability: %v", want, err)
		}
	}
}
