package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

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
		Use:          "calls <symbol-or-node-id>",
		Short:        "Walk CALLS edges from a symbol (wraps eng_get_call_chain)",
		Long:         `Walk CALLS edges. --direction=out (default) shows callees ("what does this reach"); --direction=in shows callers ("what calls this"); --direction=both unions them. Pass a symbol name or a node_id.`,
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
				params["direction"] = dir
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
			return renderGraphChain(cmd.OutOrStdout(), resp, jsonOut)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "out", "out|in|both — outgoing callees, incoming callers, or both")
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
		Use:          "blast <symbol-or-node-id>",
		Short:        "Compute blast radius for a symbol (wraps eng_get_blast_radius)",
		Long:         `Show the set of symbols transitively reached (or reaching) the seed. Use BEFORE editing an exported symbol or scoping a refactor. --direction=both (default) walks callers AND callees.`,
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
				params["direction"] = dir
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_get_blast_radius", params, &resp); err != nil {
				return fmt.Errorf("blast: %w", err)
			}
			return renderGraphChain(cmd.OutOrStdout(), resp, jsonOut)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: fan out across registered repos)")
	cmd.Flags().StringVar(&dir, "direction", "both", "out|in|both — callees, callers, or both")
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
		Use:          "changed",
		Short:        "Symbol-grain diff between two git refs (wraps eng_find_changed_symbols)",
		Long:         `Show added/removed/modified symbols between two refs. With both flags omitted, defaults to HEAD~1..HEAD.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
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
// a greppable table. Used by `veska calls` and `veska blast`.
func renderGraphChain(w io.Writer, raw json.RawMessage, jsonOut bool) error {
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
		}
		return nil
	}
	for _, n := range env.Nodes {
		fmt.Fprintf(w, "%-10s %s:%d-%d  %s\n", n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name)
	}
	if len(env.CrossRepoEdges) > 0 {
		fmt.Fprintf(w, "cross-repo edges (%d):\n", len(env.CrossRepoEdges))
		for _, e := range env.CrossRepoEdges {
			fmt.Fprintf(w, "  %s --%s--> %s in %s\n", shortID(e.SrcNodeID), e.Kind, shortID(e.DstNodeID), shortID(e.DstRepoID))
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
// ID (length 32+ hex chars). Used by the graph wrappers to decide
// whether to send node_id or symbol to the daemon.
func looksLikeNodeID(s string) bool {
	if len(s) < 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
