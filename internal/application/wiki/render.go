// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package wiki

import (
	"fmt"
	"strings"
	"time"
)

// renderGeneratedHeader prints a one-line "Generated: ISO8601" stamp +
// a refresh hint, so a Markdown page never lies about how current it is
// Zero time is rendered as "_unstamped_" to keep the
// page readable when the handler hasn't filled in GeneratedAt - the
// MCP responses share these renderers and don't necessarily set it.
func renderGeneratedHeader(b *strings.Builder, at time.Time) {
	if at.IsZero() {
		b.WriteString("_Generated: unstamped - re-run `veska wiki` to refresh._\n\n")
		return
	}
	fmt.Fprintf(b, "_Generated: %s - re-run `veska wiki` to refresh._\n\n", at.UTC().Format(time.RFC3339))
}

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
	renderGeneratedHeader(&b, r.GeneratedAt)
	b.WriteString("Files ranked by change risk: recent change frequency multiplied by blast radius.\n\n")
	if len(r.Zones) == 0 {
		// explain *why* this page is empty so the reader
		// doesn't have to read the source. The same two cases as the
		// MCP tool's degraded_reasons.
		switch {
		case r.CandidatesScanned == 0:
			b.WriteString("_No commits in the past 30 days. Hot-zone ranking is per-commit-frequency-driven - commit some changes and re-run `veska wiki`._\n")
		case r.CandidatesScored == 0:
			fmt.Fprintf(&b, "_%d file(s) changed in the last 30 days, but none have graph nodes (lockfiles, READMEs, generated assets). Nothing to rank._\n", r.CandidatesScanned)
		default:
			b.WriteString("_No hot zones yet - re-run `veska wiki` after more commits land._\n")
		}
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

func boolMark(b bool) string {
	if b {
		return "✓"
	}
	return "·"
}

// RenderEntryPoints renders an EntryPointsReport to a Markdown page. The
// output is a pure function of the report: iteration is over the
// already-sorted EntryPoints slice only, so rendering the same promoted
// state twice yields byte-identical output (no map-order leakage).
func RenderEntryPoints(r EntryPointsReport) string {
	var b strings.Builder
	b.WriteString("# Entry Points\n\n")
	renderGeneratedHeader(&b, r.GeneratedAt)
	b.WriteString("High-fan-in symbols an agent should start from: ranked by ")
	b.WriteString("inbound call count, with exported symbols and symbols having ")
	b.WriteString("adjacent tests breaking ties.\n\n")
	if len(r.EntryPoints) == 0 {
		b.WriteString("_No entry points yet - symbols appear here once the auto-link pipeline has built inbound CALLS edges. Check `veska doctor post_promotion_queue`; if pending/auto_link is large the index is still warming up._\n")
		return b.String()
	}
	b.WriteString("| Symbol | File | Kind | Inbound | Exported | Tested |\n")
	b.WriteString("| ------ | ---- | ---- | ------- | -------- | ------ |\n")
	for _, e := range r.EntryPoints {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s |\n",
			e.SymbolName, e.FilePath, e.Kind, e.InboundCount,
			boolMark(e.Exported), boolMark(e.HasAdjacentTest))
	}
	return b.String()
}
