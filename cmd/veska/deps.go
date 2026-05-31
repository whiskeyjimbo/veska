package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/depscmd"
)

// The deps command logic lives in internal/cli/depscmd; the constructors below
// are Cobra glue whose RunE bodies delegate into that package (solov2-0omh).
// The cwd→repo resolver (autoResolveRepo) stays in cmd/veska — it is shared
// across the symbol, graph, findings, and deps families — and is injected
// through the ResolveRepo seam.

// depsCmd is the `veska deps …` parent. Bare `veska deps` lists
// imported modules ranked by call-site usage (the original solov2-jlws
// behaviour); `veska deps index <module>` adds the new vendor-scan
// indexer (solov2-bchl).
func depsCmd() *cobra.Command {
	listCmd := depsListCmd()
	cmd := &cobra.Command{
		Use:          "deps",
		Short:        "Inspect and index a repo's external dependencies",
		SilenceUsage: true,
		// solov2-izh6.5: cap positional args at 1 so a bare
		// `veska deps show github.com/foo` (which silently fell through
		// to `deps list` when the parent had no Args constraint) is
		// caught as an unknown subcommand instead. The single permitted
		// positional preserves the `deps <path-or-id>` ergonomics that
		// list inherits.
		Args: cobra.MaximumNArgs(1),
	}
	// Preserve the prior `veska deps` (no subcommand) behaviour by
	// promoting `list` as the default run target. When the lone arg
	// looks like a subcommand name (alphabetic word, no path separator
	// or dot) we reject it explicitly rather than passing nonsense to
	// list's repo resolver.
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 1 && looksLikeUnknownDepsSubcommand(args[0], c) {
			return fmt.Errorf("unknown deps subcommand %q — run `veska deps --help` for available subcommands", args[0])
		}
		return listCmd.RunE(c, args)
	}
	cmd.Flags().AddFlagSet(listCmd.Flags())
	cmd.AddCommand(listCmd)
	cmd.AddCommand(depsIndexCmd())
	return cmd
}

// looksLikeUnknownDepsSubcommand reports whether arg looks like a
// subcommand name (alphabetic identifier with no path separator, no dot,
// no slash) that isn't actually registered under parent. Used to give
// 'veska deps show foo' a clear error instead of silently routing to
// 'deps list' where 'show' would be tried as a repo identifier.
func looksLikeUnknownDepsSubcommand(arg string, parent *cobra.Command) bool {
	if arg == "" || strings.ContainsAny(arg, "/.@~") {
		return false
	}
	for _, r := range arg {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' && r != '_' {
			return false
		}
	}
	for _, sub := range parent.Commands() {
		if sub.Name() == arg {
			return false
		}
	}
	return true
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
		Short:        "List external modules the repo CALLS into, ranked by call-site count (import-only modules without resolved calls do not appear yet)",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var repoArg string
			if len(args) == 1 {
				repoArg = args[0]
			}
			return depscmd.RunList(cmd.Context(), depscmd.ListParams{
				RepoArg:     repoArg,
				RepoID:      repoFlag,
				Limit:       limit,
				JSONOut:     jsonOut,
				Out:         cmd.OutOrStdout(),
				ErrOut:      cmd.ErrOrStderr(),
				ResolveRepo: autoResolveRepo,
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_list_dependencies shape)")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum rows to print (0 = no limit)")
	return cmd
}

// depsIndexCmd indexes a vendored Go module's symbols into the graph
// (solov2-bchl).
func depsIndexCmd() *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:          "index <module-path>",
		Short:        "Index a vendored Go module's symbols into the graph",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return depscmd.RunIndex(cmd.Context(), depscmd.IndexParams{
				ModulePath:  args[0],
				RepoID:      repoFlag,
				Out:         cmd.OutOrStdout(),
				ErrOut:      cmd.ErrOrStderr(),
				ResolveRepo: autoResolveRepo,
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the cwd-resolved repo)")
	return cmd
}
