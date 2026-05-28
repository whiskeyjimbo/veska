package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// normalizeDirection accepts the user-friendly aliases callers/callees
// (which match the help-text prose) in addition to the canonical in/out
// enum the daemon expects. solov2-5out.
func normalizeDirection(d string) string {
	switch d {
	case "callers":
		return "in"
	case "callees":
		return "out"
	}
	return d
}

// callsCmd wraps eng_get_call_chain. One command with --direction
// (out|in|both) instead of separate `callers` / `callees` verbs — the
// underlying MCP tool already takes that parameter and a single CLI
// surface keeps the help text simple. solov2-xomk parity wrapper.
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
		// description so the chained_selectors_unresolved fallback
		// guidance can't drift between CLI and MCP surfaces. solov2-izh6.20.
		Long:         mcpinfra.DescCallChain,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{}
			if looksLikeNodeID(args[0]) {
				params["node_id"] = args[0]
			} else {
				params["symbol"] = args[0]
			}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if dir != "" {
				params["direction"] = normalizeDirection(dir)
			}
			if depth > 0 {
				params["depth"] = depth
			}
			if expandXR {
				params["expand_cross_repo"] = true
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_get_call_chain", params, &resp); err != nil {
				return fmt.Errorf("calls: %w", err)
			}
			return renderGraphChain(cmd.Context(), cmd.OutOrStdout(), resp, jsonOut)
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
			params := map[string]any{}
			if looksLikeNodeID(args[0]) {
				params["node_id"] = args[0]
			} else {
				params["symbol"] = args[0]
			}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			}
			if dir != "" {
				params["direction"] = normalizeDirection(dir)
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_get_blast_radius", params, &resp); err != nil {
				return fmt.Errorf("blast: %w", err)
			}
			return renderGraphChain(cmd.Context(), cmd.OutOrStdout(), resp, jsonOut)
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

The --ref-a/--ref-b flags remain accepted and take precedence over positional args (solov2-izh6.4).`,
		Args:         cobra.MaximumNArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// solov2-izh6.4: positional refs are common muscle memory from
			// `git diff REF_A REF_B`. Previously these were silently
			// ignored and the command fell through to the empty-tree
			// fallback, which looked like a daemon bug. Map positionals
			// onto the flag values when the flags aren't already set.
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
			if err := callMCP(cmd.Context(), "eng_find_changed_symbols", params, &resp); err != nil {
				return fmt.Errorf("changed: %w", err)
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				var pretty any
				_ = json.Unmarshal(resp, &pretty)
				return enc.Encode(pretty)
			}
			return renderChangedSymbols(cmd.OutOrStdout(), resp)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().StringVar(&refA, "ref-a", "", "base ref (default: HEAD~1)")
	cmd.Flags().StringVar(&refB, "ref-b", "", "head ref (default: HEAD)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_changed_symbols shape)")
	return cmd
}

// renderGraphChain prints a {nodes, edges, cross_repo_edges} envelope as
// a greppable table. Used by `veska calls` and `veska blast`. ctx feeds
// the eng_get_node lookups that resolve cross-repo edge endpoints from
// opaque hex to "symbol in file:line" form (solov2-y59h) — mirrors the
// resolution `veska context` already does.
func renderGraphChain(ctx context.Context, w io.Writer, raw json.RawMessage, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var env struct {
		Nodes []struct {
			NodeID    string `json:"node_id"`
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start"`
			LineEnd   int    `json:"line_end"`
			RepoID    string `json:"repo_id,omitempty"`
		} `json:"nodes"`
		Edges []struct {
			SrcNodeID string `json:"src_node_id"`
			DstNodeID string `json:"dst_node_id"`
			Kind      string `json:"kind"`
		} `json:"edges"`
		CrossRepoEdges  []CrossRepoEdgeDTO `json:"cross_repo_edges,omitempty"`
		DegradedReasons []string           `json:"degraded_reasons,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Nodes) == 0 && len(env.CrossRepoEdges) == 0 {
		fmt.Fprintln(w, "no nodes in chain")
		for _, d := range env.DegradedReasons {
			fmt.Fprintf(w, "[degraded: %s]\n", d)
			if d == "chained_selectors_unresolved" {
				// solov2-4soa: empty chain + chained_selectors_unresolved is
				// the common "I called veska calls on a cobra command body"
				// outcome. The bare degraded tag reads as "veska broke";
				// surface what it actually means so a junior knows to try
				// `veska blast` or `veska context` instead.
				fmt.Fprintln(w, "  hint: parser can't resolve chained selector expressions (e.g. rootCmd.AddCommand(...).Execute()),")
				fmt.Fprintln(w, "        so call edges from cobra-style top-level var initialisers are attributed to the package node.")
				fmt.Fprintln(w, "        try `veska blast <symbol>` or `veska context <symbol>` for a graph-wide view.")
			}
			if d == "external_callees_only" {
				// solov2-izh6.22: the symbol's callees are stdlib /
				// unregistered modules, not a parser limitation. Tell the
				// reader so they don't conclude something's broken.
				fmt.Fprintln(w, "  hint: this symbol only calls into stdlib or unregistered modules, so there are no")
				fmt.Fprintln(w, "        edges to follow in the indexed graph. try `veska blast <symbol>` for callers,")
				fmt.Fprintln(w, "        or register the dependency repo so its symbols become first-class nodes.")
			}
		}
		return nil
	}
	// Build a local lookup so cross-repo edges whose src/dst is already in
	// the response envelope can be resolved without a round-trip per side.
	localByID := make(map[string]crossRepoNodeInfo, len(env.Nodes))
	for _, n := range env.Nodes {
		if n.NodeID == "" {
			continue
		}
		localByID[n.NodeID] = crossRepoNodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, Line: n.LineStart}
		fmt.Fprintf(w, "%-10s %s:%d-%d  %s\n", n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name)
	}
	if len(env.CrossRepoEdges) > 0 {
		fmt.Fprintf(w, "cross-repo edges (%d):\n", len(env.CrossRepoEdges))
		anyPackageSrc := false
		for _, e := range env.CrossRepoEdges {
			src, ok := localByID[e.SrcNodeID]
			if !ok || src.Name == "" {
				src = resolveCrossRepoNode(ctx, e.SrcNodeID, "", "")
			}
			dst, ok := localByID[e.DstNodeID]
			if !ok || dst.Name == "" {
				dst = resolveCrossRepoNode(ctx, e.DstNodeID, e.DstRepoID, e.DstBranch)
			}
			if src.Kind == "package" {
				anyPackageSrc = true
			}
			fmt.Fprintf(w, "  %s --%s--> %s in %s\n",
				formatCrossRepoNode(src, e.SrcNodeID), e.Kind,
				formatCrossRepoNode(dst, e.DstNodeID), shortID(e.DstRepoID))
		}
		if anyPackageSrc {
			// solov2-urqy: cross-repo CALLS landing on a `package` src
			// usually means the call lives inside an anonymous function in a
			// top-level var initialiser (e.g. cobra's `Run: func(...){...}`).
			// The parser attributes those to the package node, so the user
			// can't tell which function actually makes the call.
			fmt.Fprintln(w, "  note: package-grain src means the caller is an anonymous func in a top-level var initialiser (e.g. cobra Run/RunE).")
			fmt.Fprintln(w, "        run `veska context <caller-symbol>` or grep that file for the dst symbol to pinpoint the call site.")
		}
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
	return nil
}

// CrossRepoEdgeDTO mirrors the daemon's CrossRepoEdge JSON shape locally
// so renderGraphChain can decode without importing the MCP package
// (cmd/veska is layered above internal/infrastructure).
type CrossRepoEdgeDTO struct {
	SrcNodeID string `json:"src_node_id"`
	DstNodeID string `json:"dst_node_id"`
	DstRepoID string `json:"dst_repo_id"`
	DstBranch string `json:"dst_branch"`
	Kind      string `json:"kind"`
}

// renderChangedSymbols prints the added/removed/modified buckets from
// eng_find_changed_symbols.
func renderChangedSymbols(w io.Writer, raw json.RawMessage) error {
	var env struct {
		Added           []symRow `json:"added"`
		Removed         []symRow `json:"removed"`
		Modified        []symRow `json:"modified"`
		DegradedReasons []string `json:"degraded_reasons,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Added)+len(env.Removed)+len(env.Modified) == 0 {
		fmt.Fprintln(w, "no symbol-grain changes")
	}
	for _, r := range env.Added {
		fmt.Fprintln(w, formatSymRow("+", r))
	}
	for _, r := range env.Removed {
		fmt.Fprintln(w, formatSymRow("-", r))
	}
	for _, r := range env.Modified {
		fmt.Fprintln(w, formatSymRow("~", r))
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
		if d == "baseline_ref_not_indexed" {
			// solov2-izh6.17: the bare reason just renames the problem.
			// Tell the user what it actually means — the baseline ref's
			// tree was unreadable, so the diff is empty because we never
			// saw it, not because nothing changed.
			fmt.Fprintln(w, "  hint: ref_a's tree was unreadable (e.g. an unfetched commit or unindexed baseline),")
			fmt.Fprintln(w, "        so the empty diff means 'we never saw the baseline', not 'nothing changed'.")
			fmt.Fprintln(w, "        try `git fetch` and re-run, or pick a ref_a that resolves locally.")
		}
	}
	return nil
}

type symRow struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// formatSymRow prints one changed-symbol row with mark prefix. When line
// info is missing (older daemons, or kinds the parser doesn't position)
// the ":0-0" suffix is dropped so the output stays readable.
func formatSymRow(mark string, r symRow) string {
	if r.LineStart == 0 && r.LineEnd == 0 {
		return fmt.Sprintf("%s %-10s %s  %s", mark, r.Kind, r.FilePath, r.Name)
	}
	return fmt.Sprintf("%s %-10s %s:%d-%d  %s", mark, r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name)
}

// looksLikeNodeID reports whether s looks like a hex content-hash node
// ID. The threshold is 12 hex chars — the width of the display short_id
// the CLI prints (see shortID in symbol.go) — so a user copy-pasting the
// "(66f083714906)" suffix from `veska symbol` output into `veska calls`
// or `veska blast` is routed through the node_id path and prefix-expanded
// daemon-side, instead of being misread as a symbol name (solov2-izh6.1).
func looksLikeNodeID(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
