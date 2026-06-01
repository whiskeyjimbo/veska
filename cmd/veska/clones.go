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
		nearFlag   bool
		minScore   float64
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "clones",
		Short:        "Find duplicate code: exact byte-identical clones, or --near fuzzy clusters (wraps eng_find_clones)",
		Long:         "Default: list groups of >=2 symbols whose source text is byte-for-byte identical (literal copy-paste), via content_hash equality — deterministic, no embeddings. With --near: cluster symbols whose persisted SIMILAR_TO similarity exceeds a threshold above auto-link's 'related' cutoff (fuzzy near-duplicates — renamed copies, drifted variants), reading scores auto-link already stored. For 'what else looks like this ONE symbol?', use `veska similar <symbol>`. Note: --near needs SIMILAR_TO edges carrying scores; reindex a repo promoted before scoring landed.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return clonescmd.Run(cmd.Context(), clonescmd.Params{
				RepoID:   repoFlag,
				Branch:   branchFlag,
				Near:     nearFlag,
				MinScore: minScore,
				JSONOut:  jsonOut,
				Out:      cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo's active branch)")
	cmd.Flags().BoolVar(&nearFlag, "near", false, "fuzzy near-duplicate clusters from SIMILAR_TO edges (default: exact clones)")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "with --near: minimum similarity score (0 = provisional default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_clones shape)")
	return cmd
}
