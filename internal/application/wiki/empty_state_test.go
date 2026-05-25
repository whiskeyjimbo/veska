package wiki

import (
	"strings"
	"testing"
)

// solov2-gdf1 + solov2-z5o0 verification — empty hot_zones and
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
