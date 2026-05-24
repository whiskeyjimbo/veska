package wiki

import (
	"strings"
	"testing"
)

// solov2-gdf1 verification — empty hot_zones and entry_points pages
// render the friendlier "appears here once …" copy, not the old
// "no files changed" / "no symbols qualify" lines that read as
// "feature broken" on a fresh repo.

func TestRenderHotZones_EmptyStateMentionsCommitsAndWiki(t *testing.T) {
	out := RenderHotZones(Report{RepoID: "r1", Branch: "main"})
	for _, want := range []string{"commits land", "veska wiki"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty hot_zones page missing %q\n--- got ---\n%s", want, out)
		}
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
