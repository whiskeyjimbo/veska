package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// depsCmd wraps eng_list_dependencies so users can see external-module
// usage from the CLI without crafting JSON-RPC. The shape mirrors
// `veska symbol` / `veska context` — autoResolveRepo when a single repo
// is registered, --repo to scope across multiple, --json for raw output
// (solov2-jlws).
func depsCmd() *cobra.Command {
	var (
		repoFlag string
		jsonOut  bool
		limit    int
	)
	cmd := &cobra.Command{
		Use:          "deps",
		Short:        "List external modules the repo imports, ranked by call-site usage",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := map[string]any{}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			} else if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
				params["repo_id"] = rid
			}
			var resp struct {
				Dependencies []struct {
					Module       string `json:"module"`
					Version      string `json:"version,omitempty"`
					Language     string `json:"language"`
					UsageCount   int    `json:"usage_count"`
					TopCallSites []struct {
						SrcNodeID  string `json:"src_node_id"`
						SymbolPath string `json:"symbol_path"`
					} `json:"top_call_sites"`
				} `json:"dependencies"`
			}
			if err := callMCP(cmd.Context(), "eng_list_dependencies", params, &resp); err != nil {
				return fmt.Errorf("deps: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			if len(resp.Dependencies) == 0 {
				fmt.Fprintln(w, "no external dependencies (or no calls into them yet — the graph fills in as files are promoted)")
				return nil
			}
			shown := resp.Dependencies
			truncated := 0
			if limit > 0 && len(shown) > limit {
				truncated = len(shown) - limit
				shown = shown[:limit]
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "MODULE\tVERSION\tUSAGE\tTOP_SYMBOLS")
			for _, d := range shown {
				symbols := ""
				for i, cs := range d.TopCallSites {
					if i > 0 {
						symbols += ", "
					}
					symbols += cs.SymbolPath
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", d.Module, d.Version, d.UsageCount, symbols)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if truncated > 0 {
				fmt.Fprintf(w, "... %d more (raise --limit to see all)\n", truncated)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_list_dependencies shape)")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum rows to print (0 = no limit)")
	return cmd
}
