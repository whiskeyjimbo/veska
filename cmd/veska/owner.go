// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/ownercmd"
)

// ownerCmd wraps eng_find_owner. parity wrapper. eng_find_owner
// requires repo_id, so when --repo is omitted we resolve it from the cwd
// (autoResolveRepo) before calling - the daemon can't infer it server-side
// for this tool (its schema has no cwd param).
func ownerCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "owner <path-or-symbol>",
		Short:        "Find the owner of a file via CODEOWNERS or git blame (wraps eng_find_owner)",
		Long:         "Resolve who owns a file - CODEOWNERS longest-match first, git-blame dominant-committer fallback. The argument may be a file path, a symbol, or a node_id (symbol/node resolve to their defining file).",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID := repoFlag
			if repoID == "" {
				repoID = autoResolveRepo(cmd.Context(), cmd.ErrOrStderr())
			}
			return ownercmd.Run(cmd.Context(), ownercmd.Params{
				Anchor:  args[0],
				RepoID:  repoID,
				Branch:  branchFlag,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: cwd-resolved repo)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch used when resolving a symbol/node to its file")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_owner shape)")
	return cmd
}
