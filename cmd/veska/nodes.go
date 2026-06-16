package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"
)

// nodeCmd wraps eng_get_node. parity wrapper. The node_id is the
// content-hashed sha256 (or its 12-char display prefix) that `veska symbol`,
// `veska calls`, etc. print; repo_id/branch are optional since the id is
// globally unique.
func nodeCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "node <node-id>",
		Short:        "Show a single node by id (wraps eng_get_node)",
		Long:         "Look up one node by its content-hashed node_id (or 12-char display prefix). repo_id/branch are optional — the id is globally unique; pass both to apply the staging overlay.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return graphcmd.RunNode(cmd.Context(), graphcmd.NodeParams{
				NodeID:  args[0],
				RepoID:  repoFlag,
				Branch:  branchFlag,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (optional; node_id is globally unique)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (optional; pass with --repo to apply the staging overlay)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_node shape)")
	return cmd
}

// fileNodesCmd wraps eng_get_file_nodes. parity wrapper. Returns
// every node defined in a single file (repo-relative or absolute path).
func fileNodesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "file-nodes <path>",
		Short:        "List every node defined in a file (wraps eng_get_file_nodes)",
		Long:         "Return all nodes for a single source file. The path is repo-relative when --repo is given, otherwise absolute. Staged nodes take precedence when an uncommitted version exists.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return graphcmd.RunFileNodes(cmd.Context(), graphcmd.FileNodesParams{
				FilePath: args[0],
				RepoID:   repoFlag,
				Branch:   branchFlag,
				JSONOut:  jsonOut,
				Out:      cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (path is repo-relative when set)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: active branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_file_nodes shape)")
	return cmd
}
