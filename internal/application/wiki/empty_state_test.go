package wiki

import (
	"strings"
	"testing"
	"time"
)

// + verification - empty hot_zones and
// entry_points pages render a friendly explanation, not the old "no
// files changed" / "no symbols qualify" lines that read as "feature
// broken" on a fresh repo. The hot-zone page now picks one of three
// reasons (no-recent-commits / no-scored-zones / catch-all); each must
// still mention `veska wiki` so the reader knows how to refresh.

func TestRenderHotZones_EmptyStatesMentionWiki(t *testing.T) {
	for _, c := range []struct {
		name string
		rep  Report
	}{
		{"no-recent-commits", Report{}},
		{"no-scored-zones", Report{CandidatesScanned: 3}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := RenderHotZones(c.rep)
			if !strings.Contains(out, "veska wiki") && !strings.Contains(out, "rank") {
				t.Errorf("empty hot_zones (%s) lacks any guidance:\n%s", c.name, out)
			}
		})
	}
}

func TestRenderEntryPoints_EmptyStateMentionsAutoLinkAndDoctor(t *testing.T) {
	out := RenderEntryPoints(EntryPointsReport{RepoID: "r1", Branch: "main"})
	for _, want := range []string{"auto-link", "post_promotion_queue"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty entry_points page missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestRender_StampsGeneratedAt covers: a non-zero GeneratedAt
// must surface as a "_Generated: ISO8601_" header so a Markdown page on
// disk doesn't lie about how current it is. A zero GeneratedAt renders an
// "_unstamped_" hint pointing the reader at `veska wiki`.
func TestRender_StampsGeneratedAt(t *testing.T) {
	at := time.Date(2026, 5, 26, 14, 30, 0, 0, time.UTC)
	hz := RenderHotZones(Report{RepoID: "r", Branch: "main", GeneratedAt: at})
	if !strings.Contains(hz, "2026-05-26T14:30:00Z") {
		t.Errorf("hot_zones missing stamped time:\n%s", hz)
	}
	if !strings.Contains(hz, "veska wiki") {
		t.Errorf("hot_zones header must point at refresh command:\n%s", hz)
	}
	ep := RenderEntryPoints(EntryPointsReport{RepoID: "r", Branch: "main", GeneratedAt: at})
	if !strings.Contains(ep, "2026-05-26T14:30:00Z") {
		t.Errorf("entry_points missing stamped time:\n%s", ep)
	}
	hz0 := RenderHotZones(Report{RepoID: "r", Branch: "main"})
	if !strings.Contains(hz0, "unstamped") {
		t.Errorf("zero GeneratedAt must render as 'unstamped':\n%s", hz0)
	}
}
