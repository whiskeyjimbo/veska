package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/savings"
)

// doctorSavingsCmd returns the "doctor savings" subcommand. It reads
// the per-search telemetry written by the daemon's MCP search handler
// (solov2-3bu) and renders today / 7d / all-time savings bars.
//
// "Savings" here is the ratio (1 - snippet_chars / file_chars): how
// much agent-side file-read traffic the inline snippets in
// eng_search_semantic results saved.
func doctorSavingsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "savings",
		Short:        "Show inline-snippet token savings per period",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSavings(cmd.OutOrStdout(), config.DefaultVectorDir(), time.Now(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

func runSavings(w io.Writer, veskaHome string, now time.Time, jsonOut bool) error {
	path := filepath.Join(veskaHome, "savings.jsonl")
	rep, err := savings.Aggregate(path, now)
	if err != nil {
		return fmt.Errorf("savings: %w", err)
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(rep)
	}
	if rep.AllTime.Calls == 0 {
		fmt.Fprintln(w, "savings: no search calls recorded yet")
		return nil
	}
	fmt.Fprintln(w, "savings (file_chars vs snippet_chars; higher = more agent-side reads avoided):")
	for _, p := range []savings.Period{rep.Today, rep.Last7d, rep.AllTime} {
		fmt.Fprintln(w, formatSavingsRow(p))
	}
	return nil
}

const savingsBarWidth = 30

// formatSavingsRow renders one period as a fixed-width row:
//
//	today    [████████████████████████....] 87.3%  (42 calls, 1.2MB → 156KB)
//
// The bar fill is proportional to the savings ratio; the trailing
// detail shows the raw numerator/denominator so the user can sanity
// check.
func formatSavingsRow(p savings.Period) string {
	ratio := p.SavingsRatio()
	filled := min(max(int(ratio*float64(savingsBarWidth)), 0), savingsBarWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("·", savingsBarWidth-filled)
	return fmt.Sprintf("  %-9s [%s] %5.1f%%  (%d calls, %s -> %s)",
		p.Label, bar, ratio*100, p.Calls,
		humanBytes(p.FileChars), humanBytes(p.SnippetChars))
}

// humanBytes renders n in the largest base-1024 unit that keeps the
// numeric part under 1024. Output stays narrow ("1.2KB", "873B") so
// the savings row fits comfortably in an 80-column terminal.
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/(k*k*k))
	}
}
