package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/duplicatescmd"
)

// duplicatesCmd wraps eng_find_clusters: the unified, tier-labeled
// similar-code view for de-dupe triage — exact (byte-identical), structural
// (renamed Type-2), and near (vector) clusters in one ranked pass, repo-wide or
// across all registered repos. Each grouping is shaped to become a verify-and
// dedupe task. For exact-only/near-only the older `veska clones` still works; for
// the per-symbol "what's like THIS?" pivot use `veska similar <symbol>`.
func duplicatesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		allRepos   bool
		tiers      string
		path       string
		minScore   float64
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:          "duplicates",
		Short:        "Whole-repo (or cross-repo) similar-code clusters for de-dupe triage (wraps eng_find_clusters)",
		Long:         "List groups of >=2 similar symbols in one ranked pass across three tiers (tightest first): 'exact' (byte-identical copy-paste), 'structural' (same shape after renaming variables/literals — Type-2 clones), and 'near' (vector-similar above the elected embedder's calibrated threshold). A symbol appears at most once, at its tightest tier. No seed needed — point it at a repo (or --all-repos) and turn each cluster into a verify-and-dedupe task. Note: structural/near need structural_hash + scored SIMILAR_TO edges; reindex a graph promoted before they landed.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return duplicatescmd.Run(cmd.Context(), duplicatescmd.Params{
				RepoID:   repoFlag,
				Branch:   branchFlag,
				AllRepos: allRepos,
				Tiers:    tiers,
				Path:     path,
				MinScore: minScore,
				JSONOut:  jsonOut,
				Out:      cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (ignored with --all-repos)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo's active branch, or 'main' with --all-repos)")
	cmd.Flags().BoolVar(&allRepos, "all-repos", false, "cluster across every registered repo (cross-repo; exact+structural only)")
	cmd.Flags().StringVar(&tiers, "tiers", "", "comma-separated subset of exact,structural,near (default: all)")
	cmd.Flags().StringVar(&path, "path", "", "restrict to nodes whose file_path starts with this prefix")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "near tier: minimum similarity score (0 = calibrated default; lower for more recall)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_clusters shape)")
	return cmd
}
