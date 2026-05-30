package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/findingscmd"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// The findings command tree's logic lives in internal/cli/findingscmd; the
// constructors below are Cobra glue whose RunE bodies are thin delegating
// calls into that package (solov2-0omh.7). The cwd→repo resolver
// (autoResolveRepo) stays in cmd/veska — it is shared across the symbol,
// graph, and deps families — and is injected through the ResolveRepo seam.

// findingsCmd is the parent for `veska findings …`, wrapping the
// eng_list_findings / eng_get_finding / eng_close_finding /
// eng_reopen_finding tools so users can interact with promotion-check output
// without crafting JSON-RPC payloads (solov2-16qu).
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
	cmd.AddCommand(findingsSuppressCmd())
	cmd.AddCommand(findingsSuppressionsCmd())
	return cmd
}

func findingsListCmd() *cobra.Command {
	var (
		repoFlag   string
		allRepos   bool
		state      string
		severity   string
		rule       string
		limit      int
		jsonOut    bool
		includeLow bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List findings (default state=open)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return findingscmd.RunList(cmd.Context(), findingscmd.ListParams{
				RepoID:      repoFlag,
				AllRepos:    allRepos,
				State:       state,
				Severity:    severity,
				Rule:        rule,
				Limit:       limit,
				JSONOut:     jsonOut,
				IncludeLow:  includeLow,
				Out:         cmd.OutOrStdout(),
				ErrOut:      cmd.ErrOrStderr(),
				ResolveRepo: autoResolveRepo,
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&allRepos, "all", false, "list findings across every registered repo")
	cmd.Flags().StringVar(&state, "state", "", "filter by state (open|closed; default open)")
	cmd.Flags().StringVar(&severity, "severity", "", "filter by severity")
	cmd.Flags().StringVar(&rule, "rule", "", "filter by rule (e.g. vulnerable_dependency, dead-code, secret_leak, auto-link)")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum rows to print (0 = no limit)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&includeLow, "include-low", false, "include low-severity findings (default hides auto-link noise)")
	return cmd
}

func findingsShowCmd() *cobra.Command {
	var (
		repoFlag string
		branch   string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "show <finding_id>",
		Short:        "Show a single finding by id",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return findingscmd.RunShow(cmd.Context(), findingscmd.ShowParams{
				FindingID: args[0],
				RepoID:    repoFlag,
				Branch:    branch,
				JSONOut:   jsonOut,
				Out:       cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (full or short) to scope the lookup; parity with `findings list`")
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
			if err := mcpclient.Call(cmd.Context(), "eng_close_finding", params, &resp); err != nil {
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
			return findingscmd.RunReopen(cmd.Context(), findingscmd.ReopenParams{
				FindingID: args[0],
				RepoID:    repoFlag,
				Branch:    branch,
				Out:       cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id")
	cmd.Flags().StringVar(&branch, "branch", "", "branch")
	return cmd
}
