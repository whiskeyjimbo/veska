// Package savingscmd holds the business logic behind `veska doctor savings`
// (and its top-level `veska savings` alias). cmd/veska delegates here, following
// the cmd = glue / logic-in-packages pattern (solov2-0omh.8).
//
// "Savings" is the ratio (1 - snippet_chars / file_chars): how much agent-side
// file-read traffic the inline snippets in eng_search_semantic results saved.
// The per-search telemetry is written by the daemon's MCP search handler
// ; this package reads the rollup and renders today / 7d / all-time.
package savingscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
)

// barWidth is the character width of the rendered savings bar.
const barWidth = 30

// minSampleCalls is the minimum number of recorded search calls before the
// savings ratio is rendered as a number. Below this, the small sample is noise
// — a single short snippet can drive the ratio negative and alarm a first-time
// user . The row still renders so the call count is visible.
const minSampleCalls = 20

// Params bundles the inputs of Run. The two boolean flags (JSON, Aggregate)
// live in a struct rather than as adjacent positional args so call sites can't
// transpose them .
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

// repoBreakdown is the --json shape for the per-repo default: each repo's
// rollup keyed by repo_id, plus the pooled Total (== sum of Repos by
// construction, see savings.AggregateByRepo). Legacy entries written before
// the repo_id partition (solov2-0ql0) carry no repo_id and group under the
// "" key.
type repoBreakdown struct {
	Repos map[string]savings.Report `json:"repos"`
	Total savings.Report            `json:"total"`
}

// Run reads the savings.jsonl rollup and renders it.
//
// Default mode (solov2-izh6.21) breaks savings down per repo — one section per
// repo that has recorded searches, plus a pooled "total" — so the user can see
// which repo's symbol embeddings are paying off. --aggregate keeps the older
// single pooled view (every repo summed into one "all repos" bucket) for
// scripts and users that want just the headline number.
//
// Repos with no recorded searches do not appear: a repo_id only lands in
// savings.jsonl once eng_search_semantic has hit it, so an idle registered
// repo has no savings story to tell.
func Run(p Params) error {
	w := p.Out
	path := filepath.Join(p.VeskaHome, "savings.jsonl")
	if p.Aggregate {
		return runAggregate(w, path, p)
	}
	return runPerRepo(w, path, p)
}

// runAggregate renders the pooled single-bucket view (--aggregate).
func runAggregate(w io.Writer, path string, p Params) error {
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
	printSection(w, "all repos", rep, p.FormatBytes)
	printWarmupNote(w, rep.AllTime.Calls)
	return nil
}

// runPerRepo renders one section per repo plus a pooled total (the default).
func runPerRepo(w io.Writer, path string, p Params) error {
	byRepo, err := savings.AggregateByRepo(path, p.Now)
	if err != nil {
		return fmt.Errorf("savings: %w", err)
	}
	total, err := savings.Aggregate(path, p.Now)
	if err != nil {
		return fmt.Errorf("savings: %w", err)
	}
	if p.JSON {
		return json.NewEncoder(w).Encode(repoBreakdown{Repos: byRepo, Total: total})
	}
	if total.AllTime.Calls == 0 {
		fmt.Fprintln(w, "savings: no search calls recorded yet")
		return nil
	}
	fmt.Fprintln(w, "savings (file_chars vs snippet_chars; higher = more agent-side reads avoided):")
	for _, id := range sortedRepoIDs(byRepo) {
		printSection(w, repoLabel(id), byRepo[id], p.FormatBytes)
	}
	printSection(w, "total", total, p.FormatBytes)
	printWarmupNote(w, total.AllTime.Calls)
	return nil
}

// sortedRepoIDs orders repos most-active-first (by all-time calls), breaking
// ties on repo_id so the output is deterministic.
func sortedRepoIDs(byRepo map[string]savings.Report) []string {
	ids := make([]string, 0, len(byRepo))
	for id := range byRepo {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ci, cj := byRepo[ids[i]].AllTime.Calls, byRepo[ids[j]].AllTime.Calls
		if ci != cj {
			return ci > cj
		}
		return ids[i] < ids[j]
	})
	return ids
}

// repoLabel renders a repo_id for the section header: a short hash prefix, or
// an explicit "(untagged)" for the legacy "" bucket (entries written before
// the repo_id partition landed).
func repoLabel(repoID string) string {
	if repoID == "" {
		return "(untagged)"
	}
	return repocmd.ShortRepoID(repoID)
}

// printSection prints a "<label>:" header followed by the three period rows.
func printSection(w io.Writer, label string, rep savings.Report, formatBytes func(int64) string) {
	fmt.Fprintf(w, "  %s:\n", label)
	for _, period := range []savings.Period{rep.Today, rep.Last7d, rep.AllTime} {
		fmt.Fprintln(w, formatRow(period, formatBytes))
	}
}

// printWarmupNote explains the "warming up" rows when the sample is still small.
// In per-repo mode each repo warms up independently, so this fires off the
// pooled total — the most generous threshold.
func printWarmupNote(w io.Writer, calls int) {
	if calls < minSampleCalls {
		fmt.Fprintf(w, "  (ratio reported once a period has >= %d calls; below that the row reads 'warming up')\n", minSampleCalls)
	}
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
