package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// resolveRepoFromCWD asks the daemon (via eng_get_current_repo) which repo
// the caller's cwd belongs to. Used by CLI wrappers (symbol, context, ...)
// to bridge the gap when the daemon has multiple repos registered and the
// user hasn't passed --repo. Empty string + no error means "couldn't
// resolve"; the caller should still pass the request through and let the
// daemon's "repo_id is required" error surface (solov2-zukc).
func resolveRepoFromCWD(ctx context.Context) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil // cwd lookup failed; don't fail the whole command
	}
	var res struct {
		Repo struct {
			RepoID   string `json:"repo_id"`
			RootPath string `json:"root_path"`
		} `json:"repo"`
	}
	if err := callMCP(ctx, "eng_get_current_repo", map[string]any{"cwd": cwd}, &res); err != nil {
		// Daemon down or no match — caller falls through with no auto-resolve.
		return "", nil
	}
	return res.Repo.RepoID, nil
}

// autoResolveRepo wraps resolveRepoFromCWD with a stderr breadcrumb so the
// user is never surprised when --repo defaulted to a repo other than the
// one they were thinking of. Multi-repo silent fallback was the
// #1 first-impression bug in the junior-journey walk-through (solov2-dqwh).
// errOut may be nil to suppress the hint (e.g. JSON-output paths where a
// stray stderr line could clutter pipelines — callers there pay the
// no-hint cost knowingly).
func autoResolveRepo(ctx context.Context, errOut io.Writer) string {
	rid, _ := resolveRepoFromCWD(ctx)
	if rid == "" {
		return ""
	}
	// Only emit the hint when we know there's more than one repo to choose
	// between — solo-repo users don't need the noise.
	var list struct {
		Repos []struct {
			RepoID   string `json:"repo_id"`
			ShortID  string `json:"short_id"`
			RootPath string `json:"root_path"`
		} `json:"repos"`
	}
	if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &list); err == nil && len(list.Repos) > 1 && errOut != nil {
		short, root := rid[:12], ""
		for _, rec := range list.Repos {
			if rec.RepoID == rid {
				if rec.ShortID != "" {
					short = rec.ShortID
				}
				root = rec.RootPath
				break
			}
		}
		fmt.Fprintf(errOut, "veska: scoped to repo %s (%s); pass --repo to override\n", short, root)
	}
	return rid
}

// symbolCmd wraps eng_find_symbol so users can drive the same lookup their
// editor would, without typing the JSON-RPC envelope. repo_id auto-resolves
// when exactly one repo is registered (the daemon's
// resolveRepoIDOrSingleton); pass --repo to scope across multiple
// (solov2-kzhe).
func symbolCmd() *cobra.Command {
	var (
		repoFlag string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "symbol <name>",
		Short: "Look up symbols by name (wraps eng_find_symbol)",
		Long: `Find symbols by unqualified name or symbol path.

Auto-resolves repo_id from the only registered repo when --repo is omitted;
pass --repo <short_id> to scope across multiple repos. Unqualified matches
are fine — "Run" finds Server.Run, Command.Run, etc., with exact matches
ranked first.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"symbol": args[0]}
			scopedRepo := ""
			if repoFlag != "" {
				params["repo_id"] = repoFlag
				scopedRepo = repoFlag
			} else if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
				// solov2-zukc: auto-resolve from cwd so a junior user inside a
				// registered repo doesn't have to look up a short_id.
				// solov2-dqwh: autoResolveRepo prints a breadcrumb when
				// multiple repos are registered.
				params["repo_id"] = rid
				scopedRepo = rid
			}
			var resp struct {
				Nodes []struct {
					NodeID    string `json:"node_id"`
					Name      string `json:"name"`
					Kind      string `json:"kind"`
					FilePath  string `json:"file_path"`
					LineStart int    `json:"line_start"`
					LineEnd   int    `json:"line_end"`
					Signature string `json:"signature,omitempty"`
					Exported  *bool  `json:"exported,omitempty"`
					External  bool   `json:"external,omitempty"`
				} `json:"nodes"`
			}
			if err := callMCP(cmd.Context(), "eng_find_symbol", params, &resp); err != nil {
				return fmt.Errorf("symbol: %w", err)
			}
			// solov2-zgwd: when the scoped probe is empty, ask every other
			// registered repo whether the symbol lives there. Non-empty in
			// the original scope short-circuits — we never re-walk the
			// registry for a happy result.
			if len(resp.Nodes) == 0 && !jsonOut && scopedRepo != "" {
				printCrossRepoSymbolHint(cmd.Context(), cmd.ErrOrStderr(), args[0], scopedRepo)
			}
			return renderNodeList(cmd.OutOrStdout(), resp, jsonOut)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_symbol shape)")
	return cmd
}

// printCrossRepoSymbolHint walks every other registered repo and prints
// a one-line hint when the symbol exists somewhere else (solov2-zgwd).
// Stays best-effort: any per-repo error is silently skipped — a stuck repo
// must not turn a successful empty result into a noisy banner. The hint
// only fires when there's at least one cross-repo match, so the "no
// matches anywhere" case is unchanged.
func printCrossRepoSymbolHint(ctx context.Context, errOut io.Writer, symbol, scopedRepoID string) {
	type repoView struct {
		RepoID  string `json:"repo_id"`
		ShortID string `json:"short_id"`
	}
	var lr struct {
		Repos []repoView `json:"repos"`
	}
	if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
		return
	}
	type otherHit struct {
		shortID string
		count   int
	}
	var others []otherHit
	for _, r := range lr.Repos {
		if r.RepoID == scopedRepoID || r.ShortID == scopedRepoID {
			continue
		}
		var probe struct {
			Nodes []struct{} `json:"nodes"`
		}
		params := map[string]any{"symbol": symbol, "repo_id": r.RepoID}
		if err := callMCP(ctx, "eng_find_symbol", params, &probe); err != nil {
			continue
		}
		if len(probe.Nodes) > 0 {
			id := r.ShortID
			if id == "" {
				id = r.RepoID
			}
			others = append(others, otherHit{shortID: id, count: len(probe.Nodes)})
		}
	}
	if len(others) == 0 {
		return
	}
	parts := make([]string, 0, len(others))
	for _, h := range others {
		parts = append(parts, fmt.Sprintf("%d in %s", h.count, h.shortID))
	}
	fmt.Fprintf(errOut, "  hint: %q has no matches here, but matches elsewhere — %s (re-run with --repo <id>)\n", symbol, strings.Join(parts, ", "))
}

func renderNodeList(w io.Writer, resp any, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	// Re-marshal+unmarshal into a generic shape so this works for either
	// {nodes:[...]} or {entries:[...]} envelopes without a dedicated type.
	raw, _ := json.Marshal(resp)
	var any struct {
		Nodes []struct {
			NodeID    string `json:"node_id"`
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start"`
			LineEnd   int    `json:"line_end"`
			External  bool   `json:"external,omitempty"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &any); err != nil {
		return err
	}
	if len(any.Nodes) == 0 {
		fmt.Fprintln(w, "no matches")
		return nil
	}
	for _, n := range any.Nodes {
		extMark := ""
		if n.External {
			extMark = " [external]"
		}
		fmt.Fprintf(w, "%-10s %s:%d-%d  %s  (%s)%s\n", n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name, n.NodeID[:12], extMark)
	}
	return nil
}

// shortID returns the first 12 hex chars of a content-hashed node ID for
// display, leaving shorter or empty inputs untouched.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// crossRepoNodeInfo is the projection resolveCrossRepoNode returns for
// the CLI's cross-repo edge rendering: the symbol name, the kind (so a
// package-grain edge can be visibly labelled as such), and a file:line
// hint so the user can navigate to the call site (solov2-358v).
type crossRepoNodeInfo struct {
	Name     string
	Kind     string
	FilePath string
	Line     int
}

// resolveCrossRepoNode best-effort resolves any cross-repo node_id (src
// or dst) to its symbol name + file location via eng_get_node, so the
// CLI can print "RunE in cmd/root.go:18 --CALLS--> greetlib.Greeter.Hello"
// instead of opaque hashes (extends solov2-7xrw, solov2-80hh; rendering
// upgrade for solov2-358v). Pass repoID/branch empty to let the daemon
// scan all (repo, branch) pairs — needed for inbound src nodes whose
// containing repo isn't on the response envelope. Returns the zero
// value on any error or empty result so a stuck remote repo never
// fails the primary context output.
func resolveCrossRepoNode(ctx context.Context, nodeID, repoID, branch string) crossRepoNodeInfo {
	if nodeID == "" {
		return crossRepoNodeInfo{}
	}
	var resp struct {
		Nodes []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start,omitempty"`
		} `json:"nodes"`
	}
	params := map[string]any{"node_id": nodeID}
	if repoID != "" {
		params["repo_id"] = repoID
	}
	if branch != "" {
		params["branch"] = branch
	}
	if err := callMCP(ctx, "eng_get_node", params, &resp); err != nil {
		return crossRepoNodeInfo{}
	}
	if len(resp.Nodes) == 0 {
		return crossRepoNodeInfo{}
	}
	n := resp.Nodes[0]
	return crossRepoNodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, Line: n.LineStart}
}

// formatCrossRepoNode renders one side of a cross-repo edge: the
// symbol name, optionally with a "package " prefix when the resolved
// node is a Go package (signalling the parser attributed the call to
// the package because it couldn't bind the specific function), and
// appended with "in file_path[:line]" when known. Falls back to a
// short-id slice of the raw node_id when nothing resolves so the row
// still has *some* identifier.
func formatCrossRepoNode(info crossRepoNodeInfo, fallbackID string) string {
	name := info.Name
	if name == "" {
		return shortID(fallbackID)
	}
	if info.Kind == "package" {
		name = "package " + name
	}
	if info.FilePath != "" {
		if info.Line > 0 {
			return fmt.Sprintf("%s in %s:%d", name, info.FilePath, info.Line)
		}
		return fmt.Sprintf("%s in %s", name, info.FilePath)
	}
	return name
}

// contextCmd wraps eng_get_context_pack so users can pull the same
// caller+callee+test bundle the agent would, without crafting JSON
// (solov2-kzhe).
func contextCmd() *cobra.Command {
	var (
		repoFlag   string
		jsonOut    bool
		symbolFlag string
	)
	cmd := &cobra.Command{
		Use:   "context <symbol>",
		Short: "Bundle a symbol with its callers/callees/tests (wraps eng_get_context_pack)",
		Long: `Print the context pack for a symbol: the seed node plus surrounding
callers, callees, and adjacent tests. Useful at the start of a non-trivial
change so you (or an agent) get the whole neighbourhood in one shot.`,
		// solov2-bvis: accept the symbol as either a positional arg or
		// a --symbol flag. The MCP tool's JSON param is "symbol" so
		// users naturally try --symbol; reject only when both or neither
		// are supplied.
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sym string
			switch {
			case len(args) == 1 && symbolFlag == "":
				sym = args[0]
			case len(args) == 0 && symbolFlag != "":
				sym = symbolFlag
			case len(args) == 1 && symbolFlag != "":
				return fmt.Errorf("context: pass symbol as positional arg OR --symbol, not both")
			default:
				return fmt.Errorf("context: a symbol is required (positional or --symbol)")
			}
			params := map[string]any{"symbol": sym}
			if repoFlag != "" {
				params["repo_id"] = repoFlag
			} else if rid := autoResolveRepo(cmd.Context(), cmd.ErrOrStderr()); rid != "" {
				// solov2-zukc: auto-resolve from cwd so a junior user inside a
				// registered repo doesn't have to look up a short_id.
				// solov2-dqwh: hint via stderr when multiple repos are registered.
				params["repo_id"] = rid
			}
			var resp json.RawMessage
			if err := callMCP(cmd.Context(), "eng_get_context_pack", params, &resp); err != nil {
				return fmt.Errorf("context: %w", err)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				var pretty any
				_ = json.Unmarshal(resp, &pretty)
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(pretty)
			}
			// Text mode: render seed + one-line-per-neighbour. The shape is
			// {mode, query, nodes:[{name, kind, file_path, distance, seed, snippet}],
			// cross_repo_edges:[{src_node_id, dst_node_id, dst_repo_id, kind}]}.
			var pack struct {
				Mode  string `json:"mode"`
				Query string `json:"query"`
				Nodes []struct {
					NodeID   string `json:"node_id"`
					Name     string `json:"name"`
					Kind     string `json:"kind"`
					FilePath string `json:"file_path"`
					Distance int    `json:"distance"`
					Seed     bool   `json:"seed"`
					Snippet  string `json:"snippet,omitempty"`
				} `json:"nodes"`
				CrossRepoEdges []struct {
					SrcNodeID string `json:"src_node_id"`
					DstNodeID string `json:"dst_node_id"`
					DstRepoID string `json:"dst_repo_id"`
					DstBranch string `json:"dst_branch"`
					Kind      string `json:"kind"`
				} `json:"cross_repo_edges,omitempty"`
			}
			if err := json.Unmarshal(resp, &pack); err != nil {
				return err
			}
			// solov2-ub9c: a zero-node pack means the symbol didn't resolve.
			// Say so plainly + point to `veska symbol` for fuzzier lookup
			// instead of the deadpan "context for X (0 node(s))".
			if len(pack.Nodes) == 0 {
				fmt.Fprintf(w, "no symbol named %q found in this repo\n", pack.Query)
				fmt.Fprintf(w, "hint: try `veska symbol %s` to fuzzy-search, or check --repo\n", pack.Query)
				return nil
			}
			fmt.Fprintf(w, "context for %s (%d node(s))\n", pack.Query, len(pack.Nodes))
			for _, n := range pack.Nodes {
				mark := " "
				if n.Seed {
					mark = "*"
				}
				fmt.Fprintf(w, " %s d=%d %-10s %s  %s\n", mark, n.Distance, n.Kind, n.Name, n.FilePath)
			}
			// solov2-7xrw: cross-repo edges that the daemon resolved through
			// cross_repo_edge_stubs. Surface them so the multi-repo journey
			// ("what does Run touch in greetlib?") doesn't dead-end at the
			// current repo's boundary. byNodeID lets us label each edge with
			// the source symbol the user actually recognises.
			if len(pack.CrossRepoEdges) > 0 {
				// solov2-358v: label cross-repo edges with the calling
				// function/method + file:line whenever the graph has it,
				// instead of the bare package name. When the caller is a
				// package node (parser couldn't bind the specific function
				// — see solov2-9rc2 for the underlying parser limitation),
				// prefix with "package " so the user knows the edge is
				// attributed at package grain.
				localByID := make(map[string]crossRepoNodeInfo, len(pack.Nodes))
				for _, n := range pack.Nodes {
					if n.NodeID == "" {
						continue
					}
					localByID[n.NodeID] = crossRepoNodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath}
				}
				fmt.Fprintf(w, "cross-repo edges (%d):\n", len(pack.CrossRepoEdges))
				for _, e := range pack.CrossRepoEdges {
					src, ok := localByID[e.SrcNodeID]
					if !ok || src.Name == "" {
						src = resolveCrossRepoNode(cmd.Context(), e.SrcNodeID, "", "")
					}
					dst, ok := localByID[e.DstNodeID]
					if !ok || dst.Name == "" {
						dst = resolveCrossRepoNode(cmd.Context(), e.DstNodeID, e.DstRepoID, e.DstBranch)
					}
					dstRepo := e.DstRepoID
					if len(dstRepo) > 12 {
						dstRepo = dstRepo[:12]
					}
					fmt.Fprintf(w, "   %s --%s--> %s in %s\n",
						formatCrossRepoNode(src, e.SrcNodeID), e.Kind,
						formatCrossRepoNode(dst, e.DstNodeID), dstRepo)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_context_pack shape)")
	cmd.Flags().StringVar(&symbolFlag, "symbol", "", "symbol name (alternative to the positional arg)")
	return cmd
}
