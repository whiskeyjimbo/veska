package wiki

import (
	"fmt"
	"strings"
)

// HotZonesPagePath is the repoRoot-relative path the hot_zone Markdown
// page is written to. Callers that wire regeneration (a separate task)
// resolve this against the repo working tree.
const HotZonesPagePath = "docs/veska/hot_zones.md"

// RenderHotZones renders a Report to a Markdown page. The output is a
// pure function of the Report: iteration is over the already-sorted
// Zones slice only, so rendering the same promoted state twice yields
// byte-identical output (no map-order leakage).
func RenderHotZones(r Report) string {
	var b strings.Builder
	b.WriteString("# Hot Zones\n\n")
	b.WriteString("Files ranked by change risk: recent change frequency multiplied by blast radius.\n\n")
	if len(r.Zones) == 0 {
		b.WriteString("_No hot zones: no files changed in the look-back window._\n")
		return b.String()
	}
	b.WriteString("| Rank | File | Recent Changes | Blast Radius | Score |\n")
	b.WriteString("| ---- | ---- | -------------- | ------------ | ----- |\n")
	for i, z := range r.Zones {
		fmt.Fprintf(&b, "| %d | %s | %d | %d | %d |\n",
			i+1, z.FilePath, z.RecentChangeFrequency, z.BlastRadius, z.Score)
	}
	return b.String()
}

// EntryPointsPagePath is the repoRoot-relative path the entry_points
// Markdown page is written to. Callers that wire regeneration (a separate
// task) resolve this against the repo working tree.
const EntryPointsPagePath = "docs/veska/entry_points.md"

// RenderEntryPoints renders an EntryPointsReport to a Markdown page. The
// output is a pure function of the report: iteration is over the
// already-sorted EntryPoints slice only, so rendering the same promoted
// state twice yields byte-identical output (no map-order leakage).
func RenderEntryPoints(r EntryPointsReport) string {
	var b strings.Builder
	b.WriteString("# Entry Points\n\n")
	b.WriteString("Low-risk symbols a newcomer or agent can safely start from: ")
	b.WriteString("each has an adjacent test, a small blast radius, and no open findings.\n\n")
	if len(r.EntryPoints) == 0 {
		b.WriteString("_No entry points: no symbol currently meets all three safety gates._\n")
		return b.String()
	}
	b.WriteString("| Symbol | File | Kind | Blast Radius |\n")
	b.WriteString("| ------ | ---- | ---- | ------------ |\n")
	for _, e := range r.EntryPoints {
		fmt.Fprintf(&b, "| %s | %s | %s | %d |\n",
			e.SymbolName, e.FilePath, e.Kind, e.BlastRadius)
	}
	return b.String()
}
