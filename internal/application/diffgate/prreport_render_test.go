// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"strings"
	"testing"
)

// TestRenderMarkdownEmptyDoesNotPanic covers the all-zero report (the soft
// on-ramp's degenerate case: RunReport emits one and exits 0). Every section
// must render its empty-state line.
func TestRenderMarkdownEmptyDoesNotPanic(t *testing.T) {
	out := RenderMarkdown(PRReport{})
	for _, want := range []string{
		"## Veska advisory report",
		"No downstream impact",
		"No change-risk standing",
		"No open findings",
		"No changed-but-untested symbols",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty report missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderMarkdownNotesOnly is the un-indexed-repo shape: only a Note, no
// sections assembled. The Note must be surfaced prominently.
func TestRenderMarkdownNotesOnly(t *testing.T) {
	out := RenderMarkdown(PRReport{
		Notes: []string{"repo_not_indexed: index \"x\" first, e.g. `veska reindex`"},
	})
	if !strings.Contains(out, "[!NOTE]") {
		t.Errorf("notes not rendered as a callout:\n%s", out)
	}
	if !strings.Contains(out, "repo_not_indexed") {
		t.Errorf("note text missing:\n%s", out)
	}
}

func TestRenderMarkdownPopulated(t *testing.T) {
	out := RenderMarkdown(PRReport{
		Branch:       "main",
		BaseRef:      "abc",
		CandidateRef: "def",
		ChangedFiles: []string{"a.go", "b.go"},
		BlastRadius: BlastRadiusSection{
			SeedFiles: 2, NodeCount: 3, Truncated: true,
			Entries: []BlastEntry{{SymbolPath: "pkg.A", FilePath: "a.go", Kind: "function", Distance: 1}},
		},
		ChangeRisk:   []ChangeRiskFile{{FilePath: "a.go", RecentChangeFrequency: 4, BlastRadius: 3, Score: 12}},
		OpenFindings: []ReportFinding{{Severity: "high", Rule: "dead-code", FilePath: "a.go", Message: "unused"}},
		Untested:     []UntestedSymbol{{NodeID: "n1", Message: "DoThing has no test"}},
	})
	for _, want := range []string{
		"Base `abc` → candidate `def` on branch `main`. 2 file(s) changed.",
		"3 node(s) reachable from 2 changed file(s) (graph traversal truncated).",
		"| pkg.A | a.go | function | 1 |",
		"| a.go | 4 | 3 | 12 |",
		"| high | dead-code | a.go | unused |",
		"- DoThing has no test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("populated report missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderMarkdownCapsRows asserts large sections are truncated for the PR
// comment medium and the full count is named.
func TestRenderMarkdownCapsRows(t *testing.T) {
	var risk []ChangeRiskFile
	for range reportMarkdownSectionLimit + 10 {
		risk = append(risk, ChangeRiskFile{FilePath: "f.go", Score: 1})
	}
	out := RenderMarkdown(PRReport{ChangeRisk: risk})
	rows := strings.Count(out, "| f.go |")
	if rows != reportMarkdownSectionLimit {
		t.Errorf("rendered %d change-risk rows, want cap of %d", rows, reportMarkdownSectionLimit)
	}
	if !strings.Contains(out, "and 10 more") {
		t.Errorf("truncation footer missing the dropped count:\n%s", out)
	}
}

// TestRenderMarkdownEscapesTableCells guards the table structure against
// free-text findings: a pipe must be escaped and a newline collapsed, else the
// row/column breaks.
func TestRenderMarkdownEscapesTableCells(t *testing.T) {
	out := RenderMarkdown(PRReport{
		OpenFindings: []ReportFinding{{
			Severity: "med", Rule: "secret", FilePath: "a.go",
			Message: "found a|b token\non line 2",
		}},
	})
	if !strings.Contains(out, "found a\\|b token on line 2") {
		t.Errorf("table cell not escaped (pipe/newline):\n%s", out)
	}
	if strings.Contains(out, "a|b") {
		t.Errorf("unescaped pipe leaked into a table cell:\n%s", out)
	}
}
