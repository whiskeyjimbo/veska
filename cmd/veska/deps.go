package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// depsCmd is the `veska deps …` parent. Bare `veska deps` lists
// imported modules ranked by call-site usage (the original solov2-jlws
// behaviour); `veska deps index <module>` adds the new vendor-scan
// indexer (solov2-bchl).
func depsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "deps",
		Short:        "Inspect and index a repo's external dependencies",
		SilenceUsage: true,
	}
	listCmd := depsListCmd()
	// Preserve the prior `veska deps` (no subcommand) behaviour by
	// promoting `list` as the default run target. Users typing the
	// bare form keep the same output.
	cmd.RunE = listCmd.RunE
	cmd.Flags().AddFlagSet(listCmd.Flags())
	cmd.AddCommand(listCmd)
	cmd.AddCommand(depsIndexCmd())
	return cmd
}

// depsListCmd wraps eng_list_dependencies — the existing solov2-jlws
// behaviour, now available as both `veska deps` and `veska deps list`.
func depsListCmd() *cobra.Command {
	var (
		repoFlag string
		jsonOut  bool
		limit    int
	)
	cmd := &cobra.Command{
		Use:          "list [<id-or-path>]",
		Short:        "List external modules the repo imports, ranked by call-site usage",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// solov2-mtd0: accept the same identifiers `repo add` / `reindex`
			// do (path, repo_id, short_id) so the CLI is consistent. Falls
			// back to --repo, then to cwd-resolved repo.
			params := map[string]any{}
			switch {
			case len(args) == 1:
				rid, err := resolveRepoArg(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("deps: %w", err)
				}
				params["repo_id"] = rid
			case repoFlag != "":
				params["repo_id"] = repoFlag
			default:
				if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
					params["repo_id"] = rid
				}
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
				var symbols strings.Builder
				for i, cs := range d.TopCallSites {
					if i > 0 {
						symbols.WriteString(", ")
					}
					symbols.WriteString(cs.SymbolPath)
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", d.Module, d.Version, d.UsageCount, symbols.String())
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

// depsIndexCmd scans <repoRoot>/vendor/<module-path> for .go files,
// parses them, and persists the nodes with external=1 so subsequent
// eng_find_symbol / eng_get_call_chain queries can see into vendored
// dependencies (solov2-bchl).
//
// Direct-write path: opens the local SQLite directly, mirroring the
// no-daemon fallback in `veska repo add`. Phase 2 will add an MCP
// tool (eng_index_external) so the daemon coordinates writes when
// running; until then we accept the "daemon should be stopped for
// this command" caveat (the single-writer pool would reject otherwise).
func depsIndexCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:          "index <module-path>",
		Short:        "Index a vendored Go module's symbols into the graph (solov2-bchl)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			modulePath := args[0]

			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("deps index: %w", err)
			}
			defer closeFn()

			// Resolve repo: --repo wins, else autoResolveRepo, else error.
			// autoResolveRepo needs the daemon to map cwd → repo, so on
			// daemon-down systems the user must pass --repo.
			repoID := repoFlag
			if repoID == "" {
				repoID = autoResolveRepo(cmd.Context(), cmd.ErrOrStderr())
			}
			if repoID == "" {
				return errors.New("deps index: --repo <id> is required when no daemon is running to resolve cwd")
			}

			// Look up the repo's root + active branch directly from the DB.
			root, branch, err := lookupRepoRootAndBranch(cmd.Context(), db, repoID)
			if err != nil {
				return fmt.Errorf("deps index: %w", err)
			}
			if root == "" {
				return fmt.Errorf("deps index: repo %s has no root_path; was it registered without a working tree?", repoID)
			}

			graph := sqlite.NewGraphRepo(db, db)
			svc, err := extindex.NewService(treesitter.NewGoParser(), graph,
				extindex.WithExternalRepoUpserter(graph))
			if err != nil {
				return fmt.Errorf("deps index: %w", err)
			}

			res, err := svc.IndexVendorModule(cmd.Context(), repoID, branch, root, modulePath)
			if err != nil {
				if errors.Is(err, extindex.ErrModuleNotVendored) {
					return fmt.Errorf("deps index: %s is not vendored under %s/vendor/ — run `go mod vendor` first, or (phase 2) the module-cache path will cover non-vendored modules", modulePath, root)
				}
				return fmt.Errorf("deps index: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"indexed %d node(s) across %d file(s) under %s/vendor/%s%s\n",
				res.Nodes, res.Files, root, modulePath, skippedSuffix(res.Skipped))
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the cwd-resolved repo)")
	return cmd
}

// skippedSuffix renders an optional " (N file(s) skipped)" suffix.
func skippedSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d file(s) skipped due to parse errors)", n)
}

// lookupRepoRootAndBranch is a thin direct-DB lookup used by `veska
// deps index` when the daemon is offline (and so eng_get_repo isn't
// available). Accepts the full repo_id or the 12-char short prefix.
// Returns the canonical root path + active branch for repoID, or an
// error when the repo isn't registered.
func lookupRepoRootAndBranch(ctx context.Context, db *sql.DB, repoID string) (string, string, error) {
	recs, err := repo.List(ctx, db)
	if err != nil {
		return "", "", fmt.Errorf("list repos: %w", err)
	}
	for _, rec := range recs {
		if rec.RepoID == repoID || strings.HasPrefix(rec.RepoID, repoID) {
			branch := rec.ActiveBranch
			if branch == "" {
				branch = "main"
			}
			return rec.RootPath, branch, nil
		}
	}
	return "", "", fmt.Errorf("repo %q not registered (run `veska repo list` to see registered repos)", repoID)
}
