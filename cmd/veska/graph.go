package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"

	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// The calls/blast/changed command logic lives in internal/cli/graphcmd; the
// constructors below are Cobra glue whose RunE bodies are thin delegating
// calls into that package (solov2-0omh.7).

// callsCmd wraps eng_get_call_chain. One command with --direction
// (out|in|both) instead of separate `callers` / `callees` verbs — the
// underlying MCP tool already takes that parameter and a single CLI surface
// keeps the help text simple. solov2-xomk parity wrapper.
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
		// can't drift between CLI and MCP surfaces. solov2-izh6.20.
		Long:         mcpinfra.DescCallChain,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := selectorParams(args[0])
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if dir != "" {
				params["direction"] = graphcmd.NormalizeDirection(dir)
			}
			if depth > 0 {
				params["depth"] = depth
			}
			if expandXR {
				params["expand_cross_repo"] = true
			}
			var resp json.RawMessage
			if err := mcpclient.Call(cmd.Context(), "eng_get_call_chain", params, &resp); err != nil {
				return fmt.Errorf("calls: %w", err)
			}
			return graphcmd.RenderGraphChain(cmd.Context(), cmd.OutOrStdout(), resp, jsonOut)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "out", "out|in|both (aliases: callees|callers) — outgoing callees, incoming callers, or both")
	cmd.Flags().IntVar(&depth, "depth", 0, "BFS depth limit (0 = daemon default)")
	cmd.Flags().BoolVar(&expandXR, "expand-cross-repo", true, "follow CALLS edges into other registered repos")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_call_chain shape)")
	return cmd
}

// blastCmd wraps eng_get_blast_radius. solov2-xomk parity wrapper.
func blastCmd() *cobra.Command {
	var (
		repoFlag string
		dir      string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "blast <symbol-or-node-id>",
		Short: "Compute blast radius for a symbol (wraps eng_get_blast_radius)",
		// Long is shared verbatim with the eng_get_blast_radius MCP tool
		// description so the diff/dirty variants and cross-repo fan-out
		// behaviour can't drift between CLI and MCP surfaces. solov2-izh6.20.
		Long:         mcpinfra.DescBlastRadius,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := selectorParams(args[0])
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if dir != "" {
				params["direction"] = graphcmd.NormalizeDirection(dir)
			}
			var resp json.RawMessage
			if err := mcpclient.Call(cmd.Context(), "eng_get_blast_radius", params, &resp); err != nil {
				return fmt.Errorf("blast: %w", err)
			}
			return graphcmd.RenderGraphChain(cmd.Context(), cmd.OutOrStdout(), resp, jsonOut)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "both", "out|in|both (aliases: callees|callers) — callees, callers, or both")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_blast_radius shape)")
	return cmd
}

// changedCmd wraps eng_find_changed_symbols. solov2-xomk parity wrapper.
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
			// solov2-izh6.4: positional refs are common muscle memory from
			// `git diff REF_A REF_B`. Map positionals onto the flag values
			// when the flags aren't already set.
			if refA == "" && len(args) >= 1 {
				refA = args[0]
			}
			if refB == "" && len(args) >= 2 {
				refB = args[1]
			}
			params := map[string]any{}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if refA != "" {
				params["ref_a"] = refA
			}
			if refB != "" {
				params["ref_b"] = refB
			}
			var resp json.RawMessage
			if err := mcpclient.Call(cmd.Context(), "eng_find_changed_symbols", params, &resp); err != nil {
				return fmt.Errorf("changed: %w", err)
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				var pretty any
				_ = json.Unmarshal(resp, &pretty)
				return enc.Encode(pretty)
			}
			return graphcmd.RenderChangedSymbols(cmd.OutOrStdout(), resp)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().StringVar(&refA, "ref-a", "", "base ref (default: HEAD~1)")
	cmd.Flags().StringVar(&refB, "ref-b", "", "head ref (default: HEAD)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_changed_symbols shape)")
	return cmd
}

// selectorParams routes a positional selector to the node_id or symbol MCP
// param depending on whether it looks like a hex content-hash node id
// (solov2-izh6.1).
func selectorParams(arg string) map[string]any {
	if graphcmd.LooksLikeNodeID(arg) {
		return map[string]any{"node_id": arg}
	}
	return map[string]any{"symbol": arg}
}
