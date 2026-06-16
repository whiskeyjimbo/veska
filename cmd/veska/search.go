package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/searchcmd"
	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// searchCmd is the one-shot eval CLI from: clone+index+query
// in a single command, no daemon required. It is a thin wrapper around
// the in-process services the daemon also wires — Ingester, Promoter,
// EmbedWorker, VectorStorage, search.Service — bolted together for a
// synchronous one-pass run instead of long-lived goroutines.
//
//	veska search "<query>" # search existing index
//	veska search "<query>" <path> # ensure indexed, then search
//	veska search "<query>" https://github.com/x # clone, index, search
//
// All orchestration lives in internal/cli/searchcmd; this RunE only parses
// flags/positionals and delegates. The cold-scan reparser factory and the
// cwd→repo matcher are cmd-owned seams (shared with `veska reindex`) injected
// through searchcmd.RunOpts.
func searchCmd(reparserFactory reparserFactoryFunc) *cobra.Command {
	var (
		k        int
		jsonOut  bool
		repoFlag string
	)
	cmd := &cobra.Command{
		Use:   "search <query> [path-or-url]",
		Short: "Semantic search; optionally clone+index a repo first",
		// Long embeds the eng_search_semantic MCP description verbatim
		// (DescSearchSemantic) so the RRF score-range guidance — scores
		// cluster around ~0.01–0.03, use rank not absolute score to
		// compare hits — can't drift between CLI and MCP surfaces. The
		// CLI-specific positional/--repo behaviour is appended below.
		Long: mcpinfra.DescSearchSemantic + `

The optional second argument (or --repo flag) selects the repo to search:
  - omitted        — auto-detect from cwd (must be a registered repo)
  - local path     — registered local repo (absolute or relative)
  - git URL        — clones into the cache tier (~/.cache/veska/repos/<id>),
                     marks as ephemeral, indexes it, then searches

Examples:
  veska search "parse config"                            # search the repo containing cwd
  veska search "parse config" /path/to/myrepo            # search a specific registered local repo
  veska search "parse config" --repo https://github.com/x  # clone (ephemeral), index, search
`,
		Args:         cobra.RangeArgs(1, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]
			var target string
			if len(args) == 2 {
				target = args[1]
			}
			if repoFlag != "" {
				if target != "" && target != repoFlag {
					return fmt.Errorf("search: --repo and the positional target both set to different values")
				}
				target = repoFlag
			}
			return searchcmd.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), searchcmd.RunOpts{
				Query:           query,
				Target:          target,
				K:               k,
				JSONOut:         jsonOut,
				ReparserFactory: reparserFactory,
				MatchByPath:     matchByPath,
			})
		},
	}
	cmd.Flags().IntVarP(&k, "limit", "k", 10, "max results to return")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (same shape as eng_search_semantic)")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo target (path, URL, repo_id or short_id) — alias for the positional argument")
	return cmd
}
