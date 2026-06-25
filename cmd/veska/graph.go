// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"

	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// The calls/blast/changed command logic lives in internal/cli/graphcmd; the
// constructors below are Cobra glue whose RunE bodies are thin delegating
// calls into that package.

// callsCmd wraps eng_get_call_chain. One command with --direction
// (out|in|both) instead of separate `callers` / `callees` verbs - the
// underlying MCP tool already takes that parameter and a single CLI surface
// keeps the help text simple. parity wrapper.
func callsCmd() *cobra.Command {
	var (
		repoFlag string
		dir      string
		depth    int
		jsonOut  bool
		expandXR bool
	)
	cmd := &cobra.Command{
		Use:   "calls <symbol-or-node-id>",
		Short: "Walk CALLS edges from a symbol (wraps eng_get_call_chain)",
		// Long is shared verbatim with the eng_get_call_chain MCP tool
		// description so the chained_selectors_unresolved fallback guidance
		// can't drift between CLI and MCP surfaces.
		Long:         mcpinfra.DescCallChain,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return graphcmd.RunCalls(cmd.Context(), graphcmd.CallsParams{
				Selector:        args[0],
				RepoID:          repoFlag,
				Direction:       dir,
				Depth:           depth,
				ExpandCrossRepo: expandXR,
				JSONOut:         jsonOut,
				Out:             cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "out", "out|in|both (aliases: callees|callers) - outgoing callees, incoming callers, or both")
	cmd.Flags().IntVar(&depth, "depth", 0, "BFS depth limit (0 = daemon default)")
	cmd.Flags().BoolVar(&expandXR, "expand-cross-repo", true, "follow CALLS edges into other registered repos")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_call_chain shape)")
	return cmd
}

// blastCmd wraps the blast-radius tool family. A single symbol seed is the
// default; --dirty seeds from the staged overlay (eng_get_dirty_blast_radius)
// and --diff from the working-tree-vs-HEAD diff (eng_get_diff_blast_radius).
// parity wrapper; --dirty/--diff added in.
func blastCmd() *cobra.Command {
	var (
		repoFlag string
		dir      string
		jsonOut  bool
		dirty    bool
		diff     bool
	)
	cmd := &cobra.Command{
		Use:   "blast [<symbol-or-node-id> | --diff [<ref_a>..<ref_b>]]",
		Short: "Compute blast radius for a symbol, or --dirty/--diff for staged/working-tree/ranged changes",
		// Long is shared verbatim with the eng_get_blast_radius MCP tool
		// description so the diff/dirty variants and cross-repo fan-out
		// behavior can't drift between CLI and MCP surfaces.
		Long:         mcpinfra.DescBlastRadius,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, selector, refA, refB, err := blastModeFromFlags(args, dirty, diff)
			if err != nil {
				return err
			}
			return graphcmd.RunBlast(cmd.Context(), graphcmd.BlastParams{
				Mode:      mode,
				Selector:  selector,
				RepoID:    repoFlag,
				RefA:      refA,
				RefB:      refB,
				Direction: dir,
				JSONOut:   jsonOut,
				Out:       cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "both", "out|in|both (aliases: callees|callers) - callees, callers, or both")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_blast_radius shape)")
	cmd.Flags().BoolVar(&dirty, "dirty", false, "seed from the staged overlay (uncommitted, pre-commit changes)")
	cmd.Flags().BoolVar(&diff, "diff", false, "seed from a git diff: bare = working-tree vs HEAD; with a positional ref range (e.g. main..HEAD) = that range")
	return cmd
}

// blastModeFromFlags maps the positional selector and the --dirty/--diff
// flags onto a BlastMode, enforcing that exactly one seed is chosen. The
// symbol seed needs the positional; --dirty takes none; --diff takes an
// optional `ref_a.ref_b` range positional (bare --diff = working-tree vs
// HEAD). The returned refA/refB are non-empty only for a ranged --diff.
func blastModeFromFlags(args []string, dirty, diff bool) (graphcmd.BlastMode, string, string, string, error) {
	switch {
	case dirty && diff:
		return 0, "", "", "", fmt.Errorf("blast: pass only one of --dirty or --diff")
	case dirty && len(args) == 1:
		return 0, "", "", "", fmt.Errorf("blast: --dirty seeds from staged changes, not a symbol - drop the positional argument")
	case dirty:
		return graphcmd.BlastDirty, "", "", "", nil
	case diff && len(args) == 1:
		refA, refB, err := parseDiffRange(args[0])
		if err != nil {
			return 0, "", "", "", err
		}
		return graphcmd.BlastDiff, "", refA, refB, nil
	case diff:
		return graphcmd.BlastDiff, "", "", "", nil
	case len(args) == 1:
		return graphcmd.BlastSymbol, args[0], "", "", nil
	default:
		return 0, "", "", "", fmt.Errorf("blast: a symbol/node-id argument is required (or pass --dirty/--diff)")
	}
}

// parseDiffRange maps a `veska blast --diff` positional onto ref_a/ref_b,
// matching `git diff` / `veska changed` ergonomics:
//
//	main.HEAD -> ref_a=main, ref_b=HEAD
//	main. -> ref_a=main, ref_b=HEAD (empty right side defaults to HEAD)
//	main -> ref_a=main, ref_b=HEAD (bare ref = ref.HEAD)
//
// The tool requires both refs together, so a bare ref is expanded to
// ref.HEAD here rather than rejected. An empty left side is an error
// there is no working-tree concept in a ref range.
func parseDiffRange(arg string) (string, string, error) {
	refA, refB, hasRange := strings.Cut(arg, "..")
	if !hasRange {
		return arg, "HEAD", nil
	}
	if refA == "" {
		return "", "", fmt.Errorf("blast: --diff range %q has no base ref - write <ref_a>..<ref_b> (e.g. main..HEAD)", arg)
	}
	if refB == "" {
		refB = "HEAD"
	}
	return refA, refB, nil
}

// changedCmd wraps eng_find_changed_symbols. parity wrapper.
func changedCmd() *cobra.Command {
	var (
		repoFlag string
		refA     string
		refB     string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "changed [ref-a [ref-b]]",
		Short: "Symbol-grain diff between two git refs (wraps eng_find_changed_symbols)",
		Long: `Show added/removed/modified symbols between two refs. Positional args match git diff ergonomics:

  veska changed                  # HEAD~1..HEAD (default)
  veska changed v1.2.0           # v1.2.0..HEAD
  veska changed v1.2.0 v1.3.0    # v1.2.0..v1.3.0

The --ref-a/--ref-b flags remain accepted and take precedence over positional args.`,
		Args:         cobra.MaximumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// positional refs are common muscle memory from
			// `git diff REF_A REF_B`. Map positionals onto the flag values
			// when the flags aren't already set.
			if refA == "" && len(args) >= 1 {
				refA = args[0]
			}
			if refB == "" && len(args) >= 2 {
				refB = args[1]
			}
			return graphcmd.RunChanged(cmd.Context(), graphcmd.ChangedParams{
				RepoID:  repoFlag,
				RefA:    refA,
				RefB:    refB,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().StringVar(&refA, "ref-a", "", "base ref (default: HEAD~1)")
	cmd.Flags().StringVar(&refB, "ref-b", "", "head ref (default: HEAD)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_changed_symbols shape)")
	return cmd
}

// graphCmd is the parent for graph-snapshot subcommands. The structural
// query verbs (calls/blast/changed) stay top-level for muscle memory; the
// snapshot surface (export, and later serve) is grouped under `graph` because
// it is a distinct, human-facing affordance rather than an agent query.
func graphCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "graph",
		Short:        "Export and explore the code graph as a shareable snapshot",
		SilenceUsage: true,
	}
	cmd.AddCommand(graphExportCmd())
	cmd.AddCommand(graphServeCmd())
	return cmd
}

// graphExportCmd wraps graphcmd.RunExport: serialize the resolved repo's graph
// to a deterministic, shareable JSON snapshot.
func graphExportCmd() *cobra.Command {
	var repoFlag, branchFlag string
	cmd := &cobra.Command{
		Use:   "export <out.json>",
		Short: "Write a deterministic JSON snapshot of the repo's code graph",
		Long: `Serialize the repo's code graph - nodes, edges, hot zones, entry points,
and dependencies, each node carrying its summary and source - to a single
JSON file. The snapshot is deterministic (re-exporting an unchanged repo
yields byte-identical output) and safe to commit so teammates skip indexing.

It opens the local graph DB directly; no running daemon is required. The
file is the shareable contract consumed by ` + "`veska graph serve`" + `.`,
		Example:      "  veska graph export graph-snapshot.json\n  veska graph export --repo myproj /tmp/snap.json",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return graphcmd.RunExport(cmd.Context(), graphcmd.ExportParams{
				OutPath: args[0],
				RepoArg: repoFlag,
				Branch:  branchFlag,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: cwd repo, or the sole registered repo)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch to export (default: the repo's active branch)")
	return cmd
}

// graphServeCmd wraps graphcmd.RunServe: start a read-only localhost web viewer
// over a committed snapshot file or a fresh in-process export of the live DB.
func graphServeCmd() *cobra.Command {
	var (
		repoFlag, branchFlag, addrFlag string
		liveFlag                       bool
	)
	cmd := &cobra.Command{
		Use:   "serve [snapshot.json]",
		Short: "Serve a read-only web graph viewer over localhost",
		Long: `Start a local, read-only HTTP server that renders the code graph in a
browser: pan/zoom an explorable graph, click a node for its file:line,
summary, and source, and search symbols by name. Nodes are colored by
entry-point / hot-zone classification.

Given a snapshot file it serves that committed snapshot (no daemon needed).
With no path (or --live) it exports a fresh snapshot from the live DB
in-process and serves that. The server binds to localhost and has no
write endpoints.`,
		Example:      "  veska graph serve                 # live export of the current repo\n  veska graph serve graph-snapshot.json   # serve a committed snapshot",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var snap string
			if len(args) == 1 {
				snap = args[0]
			}
			return graphcmd.RunServe(cmd.Context(), graphcmd.ServeParams{
				SnapshotPath: snap,
				Live:         liveFlag,
				RepoArg:      repoFlag,
				Branch:       branchFlag,
				Addr:         addrFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "export a fresh snapshot from the live DB even when a snapshot file is given")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias for --live (default: cwd repo, or the sole registered repo)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch for --live (default: the repo's active branch)")
	cmd.Flags().StringVar(&addrFlag, "addr", "127.0.0.1:8744", "localhost bind address (host:port)")
	return cmd
}
