package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/findingscmd"
)

// findings_suppress.go wires the suppression family of MCP tools
// (eng_suppress_finding, eng_list_suppressions, eng_get_suppression,
// eng_close_suppression) onto the CLI so users do not have to craft JSON-RPC
// payloads to suppress findings — parity with `findings list / show / close /
// reopen` . The list/show rendering lives in
// internal/cli/findingscmd; these constructors are Cobra glue (solov2-0omh.7).

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
			return findingscmd.RunSuppress(cmd.Context(), findingscmd.SuppressParams{
				FindingID: args[0],
				Reason:    reason,
				Scope:     scope,
				ExpiresAt: expiresAt,
				Out:       cmd.OutOrStdout(),
			})
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
			return findingscmd.RunSuppressionsList(cmd.Context(), findingscmd.SuppressionsListParams{
				RepoID:      repoFlag,
				Branch:      branch,
				JSONOut:     jsonOut,
				Out:         cmd.OutOrStdout(),
				ErrOut:      cmd.ErrOrStderr(),
				ResolveRepo: autoResolveRepo,
			})
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
			return findingscmd.RunSuppressionsShow(cmd.Context(), args[0], jsonOut, cmd.OutOrStdout())
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
			return findingscmd.RunSuppressionsClose(cmd.Context(), args[0], repoFlag, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id for audit attribution")
	return cmd
}
