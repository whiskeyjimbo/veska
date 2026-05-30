package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/wikicmd"
)

// The wiki command logic lives in internal/cli/wikicmd; this RunE only parses
// flags/positionals (the positional/--repo merge and the --all mutual-exclusion
// rule) and delegates (solov2-0omh).

// wikiCmd returns the "wiki" Cobra command. It regenerates both wiki pages
// (hot_zones + entry_points) on demand by reusing the WorkKindWiki render
// orchestration (wiki.Handler.Handle) — the same code path the post-promotion
// queue lane runs, so the output is byte-identical.
func wikiCmd() *cobra.Command {
	var (
		repoID  string
		branch  string
		allFlag bool
	)

	cmd := &cobra.Command{
		Use:          "wiki [path|repo-id]",
		Short:        "Regenerate the veska wiki pages (hot_zones + entry_points)",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if allFlag && (repoID != "" || branch != "" || len(args) > 0) {
				return fmt.Errorf("wiki: --all is mutually exclusive with --repo/--branch and positional args")
			}
			// solov2-rtql: accept an optional positional path or repo id so
			// 'veska wiki /path/to/repo' works the same way 'veska reindex'
			// and 'veska repo add' do. The positional arg and --repo flag
			// are mutually exclusive — pick one source of truth.
			if len(args) == 1 {
				if repoID != "" {
					return fmt.Errorf("wiki: pass either a positional repo selector or --repo, not both")
				}
				repoID = args[0]
			}
			return wikicmd.Run(cmd.Context(), wikicmd.Params{
				RepoID: repoID,
				Branch: branch,
				All:    allFlag,
				Out:    cmd.OutOrStdout(),
				ErrOut: cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().StringVar(&repoID, "repo", "", "repo ID to regenerate (default: the sole registered repo)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to regenerate (default: the repo's active branch)")
	cmd.Flags().BoolVar(&allFlag, "all", false, "regenerate the wiki for every registered repo")
	return cmd
}
