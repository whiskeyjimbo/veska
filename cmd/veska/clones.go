package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/clonescmd"
)

// clonesCmd wraps eng_find_clones. solov2-wfrj. Exact-clone detection by
// content_hash equality — the deterministic, embedding-free half of duplicate
// detection. For the per-function "is THIS duplicated?" pivot, reach for
// `veska similar <symbol>` (eng_search_similar) instead.
func clonesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "clones",
		Short:        "Find exact (byte-identical) code clones (wraps eng_find_clones)",
		Long:         "List groups of >=2 symbols whose source text is byte-for-byte identical (literal copy-paste), detected by content_hash equality — deterministic, no embeddings. Use to find copy-paste worth extracting into a shared helper. This is NOT fuzzy similarity: for 'what else looks like this one symbol?', use `veska similar <symbol>`.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return clonescmd.Run(cmd.Context(), clonescmd.Params{
				RepoID:  repoFlag,
				Branch:  branchFlag,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo's active branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_clones shape)")
	return cmd
}
