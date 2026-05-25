package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// findingsCmd is the parent for `veska findings …`, wrapping the
// eng_list_findings / eng_get_finding / eng_close_finding /
// eng_reopen_finding tools so users can interact with promotion-check
// output without crafting JSON-RPC payloads (solov2-16qu).
func findingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "findings",
		Short:        "List, inspect, close, or reopen promotion-check findings",
		SilenceUsage: true,
	}
	cmd.AddCommand(findingsListCmd())
	cmd.AddCommand(findingsShowCmd())
	cmd.AddCommand(findingsCloseCmd())
	cmd.AddCommand(findingsReopenCmd())
	return cmd
}

type findingView struct {
	FindingID   string  `json:"finding_id"`
	Branch      string  `json:"branch"`
	RepoID      string  `json:"repo_id"`
	FilePath    *string `json:"file_path,omitempty"`
	Severity    string  `json:"severity"`
	SourceLayer string  `json:"source_layer"`
	Rule        string  `json:"rule"`
	Message     string  `json:"message"`
	State       string  `json:"state"`
	CreatedAt   int64   `json:"created_at"`
}

func findingsListCmd() *cobra.Command {
	var (
		repoFlag string
		state    string
		severity string
		rule     string
		limit    int
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List findings (default state=open)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			} else if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
				// solov2-dqwh: surface which repo was picked when multiple are
				// registered so users don't get silently-scoped empty results.
				params["repo_id"] = rid
			}
			if state != "" {
				params["state"] = state
			}
			if severity != "" {
				params["severity"] = severity
			}
			if rule != "" {
				params["rule"] = rule
			}
			var resp struct {
				Findings []findingView `json:"findings"`
			}
			if err := callMCP(cmd.Context(), "eng_list_findings", params, &resp); err != nil {
				return fmt.Errorf("findings list: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if len(resp.Findings) == 0 {
				fmt.Fprintln(w, "no findings")
				return nil
			}

			// solov2-7ata: severity breakdown header so 100-row dumps don't
			// hide a single critical among many mediums. Sort by severity
			// then rule for stable, scannable output.
			sortFindingsBySeverity(resp.Findings)
			counts := countSeverities(resp.Findings)
			fmt.Fprintln(w, summariseFindings(len(resp.Findings), counts))

			shown := resp.Findings
			truncated := 0
			if limit > 0 && len(shown) > limit {
				truncated = len(shown) - limit
				shown = shown[:limit]
			}

			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "FINDING_ID\tSEVERITY\tRULE\tFILE\tMESSAGE")
			for _, f := range shown {
				path := ""
				if f.FilePath != nil {
					path = *f.FilePath
				}
				msg := trimRedundantFilePrefix(f.Message, path)
				if len(msg) > 80 {
					msg = msg[:77] + "..."
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.FindingID, f.Severity, f.Rule, path, msg)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if truncated > 0 {
				fmt.Fprintf(w, "... %d more (raise --limit to see all)\n", truncated)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().StringVar(&state, "state", "", "filter by state (open|closed; default open)")
	cmd.Flags().StringVar(&severity, "severity", "", "filter by severity")
	cmd.Flags().StringVar(&rule, "rule", "", "filter by rule (e.g. vuln, dead-code, secret_leak, auto-link)")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum rows to print (0 = no limit)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
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

func sortFindingsBySeverity(fs []findingView) {
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

func countSeverities(fs []findingView) map[string]int {
	out := map[string]int{}
	for _, f := range fs {
		out[f.Severity]++
	}
	return out
}

func summariseFindings(total int, counts map[string]int) string {
	parts := []string{}
	for _, s := range []string{"critical", "high", "medium", "low", "info"} {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("showing %d finding(s)", total)
	}
	return fmt.Sprintf("showing %d finding(s): %s", total, joinComma(parts))
}

func joinComma(parts []string) string {
	return strings.Join(parts, ", ")
}

// trimRedundantFilePrefix drops a leading "<file>:<line>" / "<file> " from
// the message when the file column already shows the same file — vuln
// messages embed "go.mod:151 [GHSA-…] …" but the FILE column already says
// "go.mod" (solov2-7ata).
func trimRedundantFilePrefix(msg, file string) string {
	if file == "" {
		return msg
	}
	if !strings.HasPrefix(msg, file) {
		return msg
	}
	rest := msg[len(file):]
	// Accept "<file>:<n> ", "<file>: ", or "<file> " — trim through the
	// first space, then any leading whitespace on what remains.
	if _, after, ok := strings.Cut(rest, " "); ok {
		return strings.TrimLeft(after, " ")
	}
	return msg
}

func findingsShowCmd() *cobra.Command {
	var (
		branch  string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:          "show <finding_id>",
		Short:        "Show a single finding by id",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"finding_id": args[0]}
			if branch != "" {
				params["branch"] = branch
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_get_finding", params, &resp); err != nil {
				return fmt.Errorf("findings show: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				var pretty any
				_ = json.Unmarshal(resp, &pretty)
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(pretty)
			}
			var env struct {
				Finding findingView `json:"finding"`
			}
			if err := json.Unmarshal(resp, &env); err != nil {
				return err
			}
			renderFindingHuman(w, env.Finding)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch to scope the lookup (default: active)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func findingsCloseCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:          "close <finding_id>",
		Short:        "Close a finding with a reason",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			params := map[string]any{"finding_id": args[0], "reason": reason}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_close_finding", params, &resp); err != nil {
				return fmt.Errorf("findings close: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "closed")
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "closing reason (required)")
	return cmd
}

func findingsReopenCmd() *cobra.Command {
	var (
		repoFlag string
		branch   string
	)
	cmd := &cobra.Command{
		Use:          "reopen <finding_id>",
		Short:        "Reopen a previously-closed finding",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// eng_reopen_finding requires both repo_id and branch (its UPDATE
			// is repo-scoped). When --repo / --branch aren't passed, fetch
			// the finding first so the user doesn't have to look them up.
			params := map[string]any{"finding_id": args[0]}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if branch != "" {
				params["branch"] = branch
			}
			// eng_reopen_finding requires both repo_id and branch; if the user
			// didn't pass --branch we fall back to "main" for the eng_get_finding
			// lookup so we can autofill repo_id from the resolved row. If even
			// that lookup fails, surface a clear error pointing at the flags.
			if repoFlag == "" || branch == "" {
				lookupBranch := branch
				if lookupBranch == "" {
					lookupBranch = "main"
				}
				var resp json.RawMessage
				if err := callMCP(cmd.Context(), "eng_get_finding", map[string]any{"finding_id": args[0], "branch": lookupBranch}, &resp); err == nil {
					var env struct {
						Finding findingView `json:"finding"`
					}
					_ = json.Unmarshal(resp, &env)
					if repoFlag == "" && env.Finding.RepoID != "" {
						params["repo_id"] = env.Finding.RepoID
					}
					if branch == "" && env.Finding.Branch != "" {
						params["branch"] = env.Finding.Branch
					}
				} else {
					return fmt.Errorf("findings reopen: couldn't auto-resolve repo/branch (%v); pass --repo and --branch explicitly", err)
				}
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_reopen_finding", params, &resp); err != nil {
				return fmt.Errorf("findings reopen: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "reopened")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id")
	cmd.Flags().StringVar(&branch, "branch", "", "branch")
	return cmd
}

func renderFindingHuman(w io.Writer, f findingView) {
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
