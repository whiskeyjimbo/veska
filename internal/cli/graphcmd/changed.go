// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"encoding/json"
	"fmt"
	"io"
)

type symRow struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// RenderChangedSymbols prints the added/removed/modified buckets from
// eng_find_changed_symbols.
func RenderChangedSymbols(w io.Writer, raw json.RawMessage) error {
	var env struct {
		Added           []symRow `json:"added"`
		Removed         []symRow `json:"removed"`
		Modified        []symRow `json:"modified"`
		DegradedReasons []string `json:"degraded_reasons,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Added)+len(env.Removed)+len(env.Modified) == 0 {
		fmt.Fprintln(w, "no symbol-grain changes")
	}
	for _, r := range env.Added {
		fmt.Fprintln(w, formatSymRow("+", r))
	}
	for _, r := range env.Removed {
		fmt.Fprintln(w, formatSymRow("-", r))
	}
	for _, r := range env.Modified {
		fmt.Fprintln(w, formatSymRow("~", r))
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
		if d == "baseline_ref_not_indexed" {
			// the bare reason just renames the problem. Tell
			// the user what it actually means - the baseline ref's tree was
			// unreadable, so the diff is empty because we never saw it, not
			// because nothing changed.
			fmt.Fprintln(w, "  hint: ref_a's tree was unreadable (e.g. an unfetched commit or unindexed baseline),")
			fmt.Fprintln(w, "        so the empty diff means 'we never saw the baseline', not 'nothing changed'.")
			fmt.Fprintln(w, "        try `git fetch` and re-run, or pick a ref_a that resolves locally.")
		}
	}
	return nil
}

// formatSymRow prints one changed-symbol row with mark prefix. When line info
// is missing (older daemons, or kinds the parser doesn't position) the
// ":0-0" suffix is dropped so the output stays readable.
func formatSymRow(mark string, r symRow) string {
	if r.LineStart == 0 && r.LineEnd == 0 {
		return fmt.Sprintf("%s %-10s %s  %s", mark, r.Kind, r.FilePath, r.Name)
	}
	return fmt.Sprintf("%s %-10s %s:%d-%d  %s", mark, r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name)
}
