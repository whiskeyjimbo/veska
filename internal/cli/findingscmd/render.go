package findingscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// render emits the findings envelope as JSON or the human breakdown + table.
func (p ListParams) render(resp findingsEnvelope) error {
	w := p.Out
	if p.JSONOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	if len(resp.Findings) == 0 {
		fmt.Fprintln(w, "no findings")
		return nil
	}
	// solov2-7ata: severity breakdown header so 100-row dumps don't hide a
	// single critical among many mediums. Sort by severity then rule for
	// stable, scannable output.
	sortFindingsBySeverity(resp.Findings)
	counts := countSeverities(resp.Findings)

	shown, hiddenLow := p.filterLow(resp.Findings)
	// solov2-izh6.25: summary reflects what's actually rendered, not the
	// unfiltered total.
	fmt.Fprintln(w, summariseFindings(len(shown), len(resp.Findings), counts, resp.Findings))
	if hiddenLow > 0 {
		fmt.Fprintf(w, "  (%d low-severity hidden; pass --include-low to show)\n", hiddenLow)
	}
	truncated := 0
	if p.Limit > 0 && len(shown) > p.Limit {
		truncated = len(shown) - p.Limit
		shown = shown[:p.Limit]
	}
	// Suppress the table header when nothing will render.
	if len(shown) == 0 {
		return nil
	}
	if err := p.renderTable(w, shown); err != nil {
		return err
	}
	if truncated > 0 {
		fmt.Fprintf(w, "... %d more (raise --limit to see all)\n", truncated)
	}
	return nil
}

// filterLow drops low-severity rows unless --include-low or an explicit
// --severity selector is in play . Returns the kept rows and the
// hidden-low count.
func (p ListParams) filterLow(findings []FindingView) ([]FindingView, int) {
	if p.IncludeLow || p.Severity != "" {
		return findings, 0
	}
	kept := findings[:0]
	hiddenLow := 0
	for _, f := range findings {
		if f.Severity == "low" {
			hiddenLow++
			continue
		}
		kept = append(kept, f)
	}
	return kept, hiddenLow
}

// renderTable writes the findings table; the REPO column appears only in the
// --all (cross-repo) view.
func (p ListParams) renderTable(w io.Writer, shown []FindingView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if p.AllRepos {
		fmt.Fprintln(tw, "FINDING_ID\tSEVERITY\tRULE\tREPO\tFILE\tMESSAGE")
	} else {
		fmt.Fprintln(tw, "FINDING_ID\tSEVERITY\tRULE\tFILE\tMESSAGE")
	}
	for _, f := range shown {
		path := ""
		if f.FilePath != nil {
			path = *f.FilePath
		}
		msg := trimRedundantFilePrefix(f.Message, path)
		if len(msg) > 80 {
			msg = msg[:77] + "..."
		}
		if p.AllRepos {
			short := f.RepoID
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.FindingID, f.Severity, f.Rule, short, path, msg)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.FindingID, f.Severity, f.Rule, path, msg)
		}
	}
	return tw.Flush()
}

// severityOrder ranks severities for sortFindingsBySeverity; lower = more
// severe. Unknown severities sort last.
var severityOrder = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
	"info":     4,
}

func sortFindingsBySeverity(fs []FindingView) {
	sort.SliceStable(fs, func(i, j int) bool {
		si, oki := severityOrder[fs[i].Severity]
		sj, okj := severityOrder[fs[j].Severity]
		if !oki {
			si = 99
		}
		if !okj {
			sj = 99
		}
		if si != sj {
			return si < sj
		}
		return fs[i].Rule < fs[j].Rule
	})
}

func countSeverities(fs []FindingView) map[string]int {
	out := map[string]int{}
	for _, f := range fs {
		out[f.Severity]++
	}
	return out
}

// summariseFindings produces the human header. shown is the count the table
// will render after the low-severity filter; total is the pre-filter count so
// we can say "showing X of Y" honestly when those differ. counts/all reflect
// the FULL set so the severity breakdown stays informative even when nothing
// is rendered (solov2-izh6.25).
func summariseFindings(shown, total int, counts map[string]int, all []FindingView) string {
	// solov2-vpds: when low-severity findings are dominated by a single rule
	// (typically "auto-link" on small repos), annotate the count. Threshold is
	// 80% — if the rule mix is genuinely diverse, fall back to the unannotated
	// count.
	lowAnnotation := ""
	if counts["low"] > 0 {
		ruleCounts := map[string]int{}
		for _, f := range all {
			if f.Severity == "low" {
				ruleCounts[f.Rule]++
			}
		}
		for rule, n := range ruleCounts {
			if n*5 >= counts["low"]*4 { // ≥80%
				lowAnnotation = " " + rule
				break
			}
		}
	}
	parts := []string{}
	for _, s := range []string{"critical", "high", "medium", "low", "info"} {
		if n := counts[s]; n > 0 {
			label := fmt.Sprintf("%d %s", n, s)
			if s == "low" && lowAnnotation != "" {
				label += " (" + strings.TrimSpace(lowAnnotation) + ")"
			}
			parts = append(parts, label)
		}
	}
	head := fmt.Sprintf("showing %d finding(s)", total)
	if shown != total {
		head = fmt.Sprintf("showing %d of %d finding(s)", shown, total)
	}
	if len(parts) == 0 {
		return head
	}
	return fmt.Sprintf("%s: %s", head, strings.Join(parts, ", "))
}

// trimRedundantFilePrefix drops a leading "<file>:<line>" / "<file> " from the
// message when the file column already shows the same file — vuln messages
// embed "go.mod:151 [GHSA-…] …" but the FILE column already says "go.mod"
// .
func trimRedundantFilePrefix(msg, file string) string {
	if file == "" {
		return msg
	}
	if !strings.HasPrefix(msg, file) {
		return msg
	}
	rest := msg[len(file):]
	// Accept "<file>:<n> ", "<file>: ", or "<file> " — trim through the first
	// space, then any leading whitespace on what remains.
	if _, after, ok := strings.Cut(rest, " "); ok {
		return strings.TrimLeft(after, " ")
	}
	return msg
}

// RenderFindingHuman prints a single finding in the key:value text form used
// by `veska findings show`.
func RenderFindingHuman(w io.Writer, f FindingView) {
	fmt.Fprintf(w, "finding_id : %s\n", f.FindingID)
	fmt.Fprintf(w, "state      : %s\n", f.State)
	fmt.Fprintf(w, "severity   : %s\n", f.Severity)
	fmt.Fprintf(w, "rule       : %s\n", f.Rule)
	fmt.Fprintf(w, "source     : %s\n", f.SourceLayer)
	fmt.Fprintf(w, "branch     : %s\n", f.Branch)
	if f.FilePath != nil {
		fmt.Fprintf(w, "file       : %s\n", *f.FilePath)
	}
	// findings.created_at is Unix milliseconds; convert to RFC3339.
	fmt.Fprintf(w, "created_at : %s\n", time.UnixMilli(f.CreatedAt).UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "message    :\n  %s\n", f.Message)
}
