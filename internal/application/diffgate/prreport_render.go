// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

import (
	"fmt"
	"strings"
)

// reportMarkdownSectionLimit caps how many rows each table renders in the
// Markdown view. Unlike the JSON output (complete), the Markdown is bound for a
// GitHub PR comment with a ~65k-char ceiling, and ChangeRisk/OpenFindings/
// Untested are assembled UNCAPPED (every changed file gets a change-risk
// standing). A large PR would otherwise blow the comment out, so the render
// truncates each section and prints the full count. Blast radius is already
// capped at assembly (ReportBlastEntryLimit).
const reportMarkdownSectionLimit = 25

// RenderMarkdown renders a PRReport as a GitHub-flavored Markdown body for a PR
// comment. It is a PURE function of the report (no I/O) - same convention as
// wiki.RenderHotZones - so the same assembled report renders byte-identically.
// It never panics on a zero/all-empty report: an un-indexed repo yields a
// Notes-only report (RunReport exits 0 in that case), and every section has an
// explicit empty-state line. Notes are rendered prominently because they are the
// report's honesty mechanism (a degraded section leaves a Note rather than
// failing). The blocking-gate verdict summary is NOT part of this body - the CI
// layer assembles it from the gate runs and prepends it.
func RenderMarkdown(r PRReport) string {
	var b strings.Builder
	b.WriteString("## Veska advisory report\n\n")
	fmt.Fprintf(&b, "Base `%s` → candidate `%s` on branch `%s`. %d file(s) changed.\n\n",
		mdInline(r.BaseRef), mdInline(r.CandidateRef), mdInline(r.Branch), len(r.ChangedFiles))

	renderNotes(&b, r.Notes)
	renderBlastRadius(&b, r.BlastRadius)
	renderChangeRisk(&b, r.ChangeRisk)
	renderOpenFindings(&b, r.OpenFindings)
	renderUntested(&b, r.Untested)

	b.WriteString("\n_Advisory only - this report never blocks a merge._\n")
	return b.String()
}

// renderNotes surfaces per-section degradation as a blockquote callout. Nothing
// is written when the report assembled cleanly.
func renderNotes(b *strings.Builder, notes []string) {
	if len(notes) == 0 {
		return
	}
	b.WriteString("> [!NOTE]\n")
	b.WriteString("> Some sections could not be fully assembled:\n")
	for _, n := range notes {
		fmt.Fprintf(b, "> - %s\n", mdInline(n))
	}
	b.WriteString("\n")
}

func renderBlastRadius(b *strings.Builder, s BlastRadiusSection) {
	b.WriteString("### Blast radius\n\n")
	if len(s.Entries) == 0 {
		b.WriteString("_No downstream impact found for the changed files._\n\n")
		return
	}
	fmt.Fprintf(b, "%d node(s) reachable from %d changed file(s)", s.NodeCount, s.SeedFiles)
	if s.Truncated {
		b.WriteString(" (graph traversal truncated)")
	}
	b.WriteString(".\n\n")
	b.WriteString("| Symbol | File | Kind | Distance |\n")
	b.WriteString("| ------ | ---- | ---- | -------- |\n")
	shown := capRows(len(s.Entries))
	for _, e := range s.Entries[:shown] {
		fmt.Fprintf(b, "| %s | %s | %s | %d |\n",
			mdCell(e.SymbolPath), mdCell(e.FilePath), mdCell(e.Kind), e.Distance)
	}
	renderMore(b, len(s.Entries), shown)
}

func renderChangeRisk(b *strings.Builder, files []ChangeRiskFile) {
	b.WriteString("### Change risk\n\n")
	if len(files) == 0 {
		b.WriteString("_No change-risk standing (no indexed nodes in the changed files)._\n\n")
		return
	}
	b.WriteString("Recent change frequency × blast radius, per changed file.\n\n")
	b.WriteString("| File | Recent changes | Blast radius | Score |\n")
	b.WriteString("| ---- | -------------- | ------------ | ----- |\n")
	shown := capRows(len(files))
	for _, f := range files[:shown] {
		fmt.Fprintf(b, "| %s | %d | %d | %d |\n",
			mdCell(f.FilePath), f.RecentChangeFrequency, f.BlastRadius, f.Score)
	}
	renderMore(b, len(files), shown)
}

func renderOpenFindings(b *strings.Builder, findings []ReportFinding) {
	b.WriteString("### Open findings on touched files\n\n")
	if len(findings) == 0 {
		b.WriteString("_No open findings on the touched files._\n\n")
		return
	}
	b.WriteString("| Severity | Rule | File | Message |\n")
	b.WriteString("| -------- | ---- | ---- | ------- |\n")
	shown := capRows(len(findings))
	for _, f := range findings[:shown] {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			mdCell(f.Severity), mdCell(f.Rule), mdCell(f.FilePath), mdCell(f.Message))
	}
	renderMore(b, len(findings), shown)
}

func renderUntested(b *strings.Builder, syms []UntestedSymbol) {
	b.WriteString("### Changed but untested\n\n")
	if len(syms) == 0 {
		b.WriteString("_No changed-but-untested symbols._\n\n")
		return
	}
	b.WriteString("Changed prod symbols no test reaches (a CALLS-edge proxy, not coverage data).\n\n")
	shown := capRows(len(syms))
	for _, s := range syms[:shown] {
		fmt.Fprintf(b, "- %s\n", mdInline(s.Message))
	}
	renderMore(b, len(syms), shown)
}

// capRows returns how many rows to render: at most reportMarkdownSectionLimit.
func capRows(total int) int {
	if total > reportMarkdownSectionLimit {
		return reportMarkdownSectionLimit
	}
	return total
}

// renderMore prints the truncation footer when a section was capped, naming the
// full count so the reader knows the Markdown is a summary (the JSON is complete).
func renderMore(b *strings.Builder, total, shown int) {
	if total > shown {
		fmt.Fprintf(b, "\n_…and %d more (see the JSON report for the full list)._\n", total-shown)
	}
	b.WriteString("\n")
}

// mdCell escapes a value for a Markdown TABLE cell: a literal pipe breaks the
// column and a newline breaks the row, so both are neutralized. Free-text
// finding messages are the likely offenders.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}

// mdInline neutralizes only newlines, for values rendered outside a table (list
// items, the header line) where a pipe is harmless but a newline still corrupts
// the surrounding structure.
func mdInline(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
