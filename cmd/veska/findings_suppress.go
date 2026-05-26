package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// findings_suppress.go wires the suppression family of MCP tools
// (eng_suppress_finding, eng_list_suppressions, eng_get_suppression,
// eng_close_suppression) onto the CLI so users do not have to craft
// JSON-RPC payloads to suppress findings — parity with `findings list /
// show / close / reopen` (solov2-nwef).

type suppressionView struct {
	SuppressionID string  `json:"suppression_id"`
	Scope         string  `json:"scope"`
	Target        string  `json:"target"`
	Branch        *string `json:"branch,omitempty"`
	Rule          *string `json:"rule,omitempty"`
	Reason        string  `json:"reason"`
	ExpiresAt     *int64  `json:"expires_at,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	ActorID       string  `json:"actor_id"`
	ActorKind     string  `json:"actor_kind"`
}

// findingsSuppressCmd is `veska findings suppress <finding_id> --reason ...`.
// Wraps eng_suppress_finding. branch/repo_id are derived from the finding row.
func findingsSuppressCmd() *cobra.Command {
	var (
		reason    string
		scope     string
		expiresAt int64
	)
	cmd := &cobra.Command{
		Use:          "suppress <finding_id>",
		Short:        "Suppress a finding so it stops surfacing in list",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			params := map[string]any{"finding_id": args[0], "reason": reason}
			if scope != "" {
				params["scope"] = scope
			}
			if expiresAt != 0 {
				params["expires_at"] = expiresAt
			}
			var resp struct {
				SuppressionID string `json:"suppression_id"`
				Scope         string `json:"scope"`
				Branch        string `json:"branch"`
			}
			if err := callMCP(cmd.Context(), "eng_suppress_finding", params, &resp); err != nil {
				return fmt.Errorf("findings suppress: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "suppressed: %s (scope=%s)\n", resp.SuppressionID, resp.Scope)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "suppression reason (required)")
	cmd.Flags().StringVar(&scope, "scope", "", "scope; defaults to 'finding'")
	cmd.Flags().Int64Var(&expiresAt, "expires-at", 0, "optional Unix timestamp at which the suppression expires")
	return cmd
}

// findingsSuppressionsCmd is the parent for `veska findings suppressions …`,
// holding list/show/close subcommands. Named plural to mirror the MCP
// `eng_list_suppressions` and to read naturally next to `findings list`.
func findingsSuppressionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "suppressions",
		Short:        "List, inspect, or close finding suppressions",
		SilenceUsage: true,
	}
	cmd.AddCommand(suppressionsListCmd())
	cmd.AddCommand(suppressionsShowCmd())
	cmd.AddCommand(suppressionsCloseCmd())
	return cmd
}

func suppressionsListCmd() *cobra.Command {
	var (
		repoFlag string
		branch   string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List active suppressions",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			} else if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
				params["repo_id"] = rid
			}
			if branch != "" {
				params["branch"] = branch
			}
			var resp struct {
				Suppressions []suppressionView `json:"suppressions"`
			}
			if err := callMCP(cmd.Context(), "eng_list_suppressions", params, &resp); err != nil {
				return fmt.Errorf("findings suppressions list: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if len(resp.Suppressions) == 0 {
				fmt.Fprintln(w, "no suppressions")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SUPPRESSION_ID\tSCOPE\tTARGET\tBRANCH\tREASON")
			for _, s := range resp.Suppressions {
				br := "-"
				if s.Branch != nil {
					br = *s.Branch
				}
				reason := s.Reason
				if len(reason) > 60 {
					reason = reason[:57] + "..."
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.SuppressionID, s.Scope, s.Target, br, reason)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().StringVar(&branch, "branch", "", "filter by branch (omit to list across branches)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func suppressionsShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "show <suppression_id>",
		Short:        "Show a single suppression by id",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Suppression suppressionView `json:"suppression"`
			}
			if err := callMCP(cmd.Context(), "eng_get_suppression",
				map[string]any{"suppression_id": args[0]}, &resp); err != nil {
				return fmt.Errorf("findings suppressions show: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(resp.Suppression)
			}
			s := resp.Suppression
			fmt.Fprintf(w, "suppression_id : %s\n", s.SuppressionID)
			fmt.Fprintf(w, "scope          : %s\n", s.Scope)
			fmt.Fprintf(w, "target         : %s\n", s.Target)
			if s.Branch != nil {
				fmt.Fprintf(w, "branch         : %s\n", *s.Branch)
			}
			if s.Rule != nil {
				fmt.Fprintf(w, "rule           : %s\n", *s.Rule)
			}
			fmt.Fprintf(w, "actor          : %s (%s)\n", s.ActorID, s.ActorKind)
			fmt.Fprintf(w, "created_at     : %s\n", time.Unix(s.CreatedAt, 0).UTC().Format(time.RFC3339))
			if s.ExpiresAt != nil {
				fmt.Fprintf(w, "expires_at     : %s\n", time.Unix(*s.ExpiresAt, 0).UTC().Format(time.RFC3339))
			}
			fmt.Fprintf(w, "reason         :\n  %s\n", s.Reason)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func suppressionsCloseCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:          "close <suppression_id>",
		Short:        "Close (expire now) an active suppression",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"suppression_id": args[0]}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			var resp struct {
				SuppressionID string `json:"suppression_id"`
				ExpiresAt     int64  `json:"expires_at"`
			}
			if err := callMCP(cmd.Context(), "eng_close_suppression", params, &resp); err != nil {
				return fmt.Errorf("findings suppressions close: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "closed: %s (expires_at=%d)\n", resp.SuppressionID, resp.ExpiresAt)
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id for audit attribution")
	return cmd
}
