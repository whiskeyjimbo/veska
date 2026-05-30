package main

import (
	"strings"
	"testing"

	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// TestCLILongMatchesMCPDescription_Calls pins solov2-izh6.20: the `veska
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

// TestCLILongMatchesMCPDescription_Blast pins solov2-izh6.20: the `veska
// blast` Long help string must reuse the MCP eng_get_blast_radius
// description verbatim and reference the diff/dirty variants and the
// cross-repo fan-out behaviour.
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

// TestCLILongMatchesMCPDescription_Search pins solov2-izh6.20: the `veska
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
