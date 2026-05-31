// Package savingscmd holds the business logic behind `veska doctor savings`
// (and its top-level `veska savings` alias). cmd/veska delegates here, following
// the cmd = glue / logic-in-packages pattern (solov2-0omh.8).
//
// "Savings" is the ratio (1 - snippet_chars / file_chars): how much agent-side
// file-read traffic the inline snippets in eng_search_semantic results saved.
// The per-search telemetry is written by the daemon's MCP search handler
// (solov2-3bu); this package reads the rollup and renders today / 7d / all-time.
package savingscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/savings"
)

// barWidth is the character width of the rendered savings bar.
const barWidth = 30

// minSampleCalls is the minimum number of recorded search calls before the
// savings ratio is rendered as a number. Below this, the small sample is noise
// — a single short snippet can drive the ratio negative and alarm a first-time
// user (solov2-qjhg). The row still renders so the call count is visible.
const minSampleCalls = 20

// Params bundles the inputs of Run. The two boolean flags (JSON, Aggregate)
// live in a struct rather than as adjacent positional args so call sites can't
// transpose them (solov2-w8f9).
type Params struct {
	Out       io.Writer
	VeskaHome string
	Now       time.Time
	JSON      bool
	Aggregate bool
	// FormatBytes renders a byte count for the human rows (cmd/veska's
	// humanBytes), injected to keep one formatter across backup and savings.
	FormatBytes func(int64) string
}

// Run reads the savings.jsonl rollup and renders it.
//
// Per-repo breakdown (one row per registered repo plus a total — the goal of
// solov2-izh6.21) is gated on the recorder learning to tag each Entry with its
// repo_id; until that follow-up (solov2-0ql0) lands, every entry is in one
// unlabelled pool. The text mode therefore surfaces the pool under an explicit
// "all repos" header, and the --aggregate flag is wired up now so the future
// per-repo default has a documented opt-out path.
func Run(p Params) error {
	_ = p.Aggregate // see doc comment — single-bucket today, flag is forward-compat.
	w := p.Out
	path := filepath.Join(p.VeskaHome, "savings.jsonl")
	rep, err := savings.Aggregate(path, p.Now)
	if err != nil {
		return fmt.Errorf("savings: %w", err)
	}
	if p.JSON {
		return json.NewEncoder(w).Encode(rep)
	}
	if rep.AllTime.Calls == 0 {
		fmt.Fprintln(w, "savings: no search calls recorded yet")
		return nil
	}
	fmt.Fprintln(w, "savings (file_chars vs snippet_chars; higher = more agent-side reads avoided):")
	fmt.Fprintln(w, "  all repos:")
	for _, period := range []savings.Period{rep.Today, rep.Last7d, rep.AllTime} {
		fmt.Fprintln(w, formatRow(period, p.FormatBytes))
	}
	if rep.AllTime.Calls < minSampleCalls {
		fmt.Fprintf(w, "  (ratio reported once a period has >= %d calls; below that the row reads 'warming up')\n", minSampleCalls)
	}
	return nil
}

// formatRow renders one period as a fixed-width row:
//
//	today    [████████████████████████····] 87.3%  (42 calls, 1.2MB → 156KB)
//
// The bar fill is proportional to the savings ratio; the trailing detail shows
// the raw numerator/denominator so the user can sanity check.
func formatRow(p savings.Period, formatBytes func(int64) string) string {
	ratio := p.SavingsRatio()
	filled := min(max(int(ratio*float64(barWidth)), 0), barWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("·", barWidth-filled)
	if p.Calls < minSampleCalls {
		return fmt.Sprintf("  %-9s [%s]  warming up  (%d/%d calls, %s -> %s)",
			p.Label, bar, p.Calls, minSampleCalls,
			formatBytes(p.FileChars), formatBytes(p.SnippetChars))
	}
	return fmt.Sprintf("  %-9s [%s] %5.1f%%  (%d calls, %s -> %s)",
		p.Label, bar, ratio*100, p.Calls,
		formatBytes(p.FileChars), formatBytes(p.SnippetChars))
}
