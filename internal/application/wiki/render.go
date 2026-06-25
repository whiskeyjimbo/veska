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

// OnboardingPagePath is the repoRoot-relative path the onboarding tour page is
// written to. Like the sibling pages it is a full-overwrite, veska-owned
// generated file (no hand-edit preservation).
const OnboardingPagePath = "docs/veska/onboarding.md"

// DependencyRef is the onboarding projection of one external dependency: the
// minimum the reading path needs. The wiki Handler maps the dependencies
// service Result into this so the render stays decoupled from that DTO.
type DependencyRef struct {
	Module     string
	Version    string
	Language   string
	UsageCount int
	// TopSymbol is one representative call-target symbol_path, for color. May
	// be empty.
	TopSymbol string
}

// RenderOnboarding renders the dependency-ordered onboarding tour: entry
// points first (the vocabulary everything else calls), then hot zones (what
// changes most), then dependencies (the external ground the repo stands on).
// The output is a pure function of the three already-sorted inputs (plus the
// GeneratedAt stamp), so rendering the same promoted state twice with the same
// clock yields byte-identical output - no map-order leakage.
func RenderOnboarding(ep EntryPointsReport, hot Report, deps []DependencyRef) string {
	var b strings.Builder
	b.WriteString("# Onboarding\n\n")
	renderGeneratedHeader(&b, ep.GeneratedAt)
	b.WriteString("A reading path through this repo, ordered the way a new contributor ")
	b.WriteString("or agent should explore it: the symbols everything else calls, then ")
	b.WriteString("the files most likely to change, then the external code it leans on.\n\n")
	renderOnboardingEntryPoints(&b, ep.EntryPoints)
	renderOnboardingHotZones(&b, hot.Zones)
	renderOnboardingDependencies(&b, deps)
	return b.String()
}

func renderOnboardingEntryPoints(b *strings.Builder, eps []EntryPoint) {
	b.WriteString("## 1. Entry points - start here\n\n")
	b.WriteString("Highest-fan-in symbols. Read these first to learn the vocabulary the ")
	b.WriteString("rest of the code speaks.\n\n")
	if len(eps) == 0 {
		b.WriteString("_No entry points yet - symbols appear here once the auto-link pipeline has built inbound CALLS edges._\n\n")
		return
	}
	for _, e := range eps {
		fmt.Fprintf(b, "- `%s` - %s", e.SymbolName, locRef(e.FilePath, e.LineStart))
		if e.Summary != "" {
			fmt.Fprintf(b, " - %s", e.Summary)
		}
		fmt.Fprintf(b, " _(%d callers)_\n", e.InboundCount)
	}
	b.WriteString("\n")
}

func renderOnboardingHotZones(b *strings.Builder, zones []HotZone) {
	b.WriteString("## 2. Hot zones - read next\n\n")
	b.WriteString("Files ranked by change risk (recent change frequency multiplied by ")
	b.WriteString("blast radius). These move the most, so understanding them pays off.\n\n")
	if len(zones) == 0 {
		b.WriteString("_No hot zones yet - commit some changes and re-run `veska wiki`._\n\n")
		return
	}
	for _, z := range zones {
		fmt.Fprintf(b, "- `%s` - changed %d time(s), blast radius %d\n",
			z.FilePath, z.RecentChangeFrequency, z.BlastRadius)
	}
	b.WriteString("\n")
}

func renderOnboardingDependencies(b *strings.Builder, deps []DependencyRef) {
	b.WriteString("## 3. Dependencies - the ground you stand on\n\n")
	b.WriteString("External modules this repo calls into, ranked by usage.\n\n")
	if len(deps) == 0 {
		b.WriteString("_No external dependencies recorded yet._\n")
		return
	}
	for _, d := range deps {
		fmt.Fprintf(b, "- `%s`", d.Module)
		if d.Version != "" {
			fmt.Fprintf(b, " %s", d.Version)
		}
		if d.Language != "" {
			fmt.Fprintf(b, " (%s)", d.Language)
		}
		fmt.Fprintf(b, " - %d call site(s)", d.UsageCount)
		if d.TopSymbol != "" {
			fmt.Fprintf(b, ", e.g. `%s`", d.TopSymbol)
		}
		b.WriteString("\n")
	}
}

// locRef renders a file_path:line reference, dropping the line suffix when the
// line is unknown (0) so the link stays clean.
func locRef(filePath string, line int) string {
	if line > 0 {
		return fmt.Sprintf("`%s:%d`", filePath, line)
	}
	return fmt.Sprintf("`%s`", filePath)
}
