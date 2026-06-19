// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// TestCLILongMatchesMCPDescription_Calls pins: the `veska
// calls` Long help string must reuse the MCP eng_get_call_chain
// description verbatim so the two can't drift, ensuring CLI users learn
// about the chained_selectors_unresolved fallback the same way an MCP
// agent does.
func TestCLILongMatchesMCPDescription_Calls(t *testing.T) {
	cmd := callsCmd()
	if cmd.Long != mcpinfra.DescCallChain {
		t.Fatalf("calls Long mismatch:\n want=%q\n  got=%q", mcpinfra.DescCallChain, cmd.Long)
	}
	if !strings.Contains(cmd.Long, "chained_selectors_unresolved") {
		t.Fatalf("calls Long must mention chained_selectors_unresolved; got: %q", cmd.Long)
	}
}

// TestCLILongMatchesMCPDescription_Blast pins: the `veska
// blast` Long help string must reuse the MCP eng_get_blast_radius
// description verbatim and reference the diff/dirty variants and the
// cross-repo fan-out behavior.
func TestCLILongMatchesMCPDescription_Blast(t *testing.T) {
	cmd := blastCmd()
	if cmd.Long != mcpinfra.DescBlastRadius {
		t.Fatalf("blast Long mismatch:\n want=%q\n  got=%q", mcpinfra.DescBlastRadius, cmd.Long)
	}
	for _, want := range []string{
		"eng_get_diff_blast_radius",
		"eng_get_dirty_blast_radius",
		"cross_repo_edges",
	} {
		if !strings.Contains(cmd.Long, want) {
			t.Fatalf("blast Long must mention %q; got: %q", want, cmd.Long)
		}
	}
}

// TestCLILongMatchesMCPDescription_Search pins: the `veska
// search` Long help string must contain the MCP eng_search_semantic
// description so CLI users learn the RRF score range and that rank, not
// absolute score, is the right comparator.
func TestCLILongMatchesMCPDescription_Search(t *testing.T) {
	cmd := searchCmd(defaultReparserFactory)
	if !strings.Contains(cmd.Long, mcpinfra.DescSearchSemantic) {
		t.Fatalf("search Long must contain MCP DescSearchSemantic; got: %q", cmd.Long)
	}
	for _, want := range []string{
		"0.01",
		"0.03",
		"rank",
	} {
		if !strings.Contains(cmd.Long, want) {
			t.Fatalf("search Long must mention %q; got: %q", want, cmd.Long)
		}
	}
}

// TestCLILongMatchesMCPDescription_DepsList pins: the `veska
// deps list` Long help must reuse the MCP DescDepsImportOnlyCaveat fragment
// verbatim so the import-only-modules-are-absent rule can't drift from the
// eng_list_dependencies description. The fragment is also composed into that
// MCP description, so both surfaces are checked here.
func TestCLILongMatchesMCPDescription_DepsList(t *testing.T) {
	cmd := depsListCmd()
	if !strings.Contains(cmd.Long, mcpinfra.DescDepsImportOnlyCaveat) {
		t.Fatalf("deps list Long must contain DescDepsImportOnlyCaveat;\n want substring=%q\n            got=%q", mcpinfra.DescDepsImportOnlyCaveat, cmd.Long)
	}
}

// TestCLILongMatchesMCPDescription_Symbol pins: the `veska
// symbol` Long help must reuse the MCP DescFindSymbolMatching fragment so the
// unqualified-match / exact-first ordering rule can't drift from the
// eng_find_symbol description.
func TestCLILongMatchesMCPDescription_Symbol(t *testing.T) {
	cmd := symbolCmd()
	if !strings.Contains(cmd.Long, mcpinfra.DescFindSymbolMatching) {
		t.Fatalf("symbol Long must contain DescFindSymbolMatching;\n want substring=%q\n            got=%q", mcpinfra.DescFindSymbolMatching, cmd.Long)
	}
}

// TestCLILongMatchesMCPDescription_Context pins: the `veska
// context` Long help must equal the MCP DescContextPack fragment. Only the
// shared purpose + cross-repo behavior is pinned; the MCP-only anchor prose
// (node_id/task_id) is deliberately absent because the CLI takes only a
// symbol, so the help must not advertise inputs the command rejects.
func TestCLILongMatchesMCPDescription_Context(t *testing.T) {
	cmd := contextCmd()
	if cmd.Long != mcpinfra.DescContextPack {
		t.Fatalf("context Long mismatch:\n want=%q\n  got=%q", mcpinfra.DescContextPack, cmd.Long)
	}
	for _, unwanted := range []string{"node_id", "task_id"} {
		if strings.Contains(cmd.Long, unwanted) {
			t.Fatalf("context Long must NOT advertise MCP-only anchor %q (CLI takes a symbol only); got: %q", unwanted, cmd.Long)
		}
	}
}
