package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/wikicmd"
)

// entry-points and hot-zones are raw-query siblings of `veska wiki`: `veska
// wiki` bakes these rankings into markdown, while these verbs return the same
// data straight from the daemon (text + --json) so agents/scripts skip the
// file read. Both back onto repo_id-required MCP tools, so when --repo is
// omitted we resolve it from the cwd before calling.

func entryPointsCmd() *cobra.Command {
	var (
		repoFlag     string
		includeTests bool
		limit        int
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:          "entry-points",
		Short:        "List high-fan-in entry-point symbols (wraps eng_get_entry_points)",
		Long:         "List the high-fan-in symbols ranked by inbound call count - the natural entry points to read first when learning a repo. Exported, tested symbols rank above unexported untested ones at equal inbound count.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID := repoFlag
			if repoID == "" {
				repoID = autoResolveRepo(cmd.Context(), cmd.ErrOrStderr())
			}
			return wikicmd.RunEntryPoints(cmd.Context(), wikicmd.EntryPointsParams{
				RepoID:       repoID,
				IncludeTests: includeTests,
				Limit:        limit,
				JSONOut:      jsonOut,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: cwd-resolved repo)")
	cmd.Flags().BoolVar(&includeTests, "include-tests", false, "include Test/Benchmark/Example/Fuzz entries")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = service default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_entry_points shape)")
	return cmd
}

func hotZonesCmd() *cobra.Command {
	var (
		repoFlag string
		limit    int
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "hot-zones",
		Short:        "List files ranked by change risk (wraps eng_get_hot_zone)",
		Long:         "List the top files by change risk = recent-change-frequency × blast-radius - the load-bearing files where a small edit fans out the most. Useful during PR review or onboarding.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID := repoFlag
			if repoID == "" {
				repoID = autoResolveRepo(cmd.Context(), cmd.ErrOrStderr())
			}
			return wikicmd.RunHotZones(cmd.Context(), wikicmd.HotZonesParams{
				RepoID:  repoID,
				Limit:   limit,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: cwd-resolved repo)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max files (0 = service default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_hot_zone shape)")
	return cmd
}
