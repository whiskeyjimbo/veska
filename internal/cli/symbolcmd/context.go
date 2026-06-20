// SPDX-License-Identifier: AGPL-3.0-only

package symbolcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/whiskeyjimbo/veska/internal/cli/graphref"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// ContextParams bundles the inputs of RunContext.
type ContextParams struct {
	Symbol  string
	RepoID  string // explicit --repo; empty means daemon fan-out
	JSONOut bool
	Out     io.Writer
}

// contextPack is the eng_get_context_pack response shape the text renderer
// decodes.
type contextPack struct {
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

// RunContext wraps eng_get_context_pack: it issues the lookup and renders the
// seed + callers/callees/tests bundle (and any cross-repo edges) as JSON or
// text. Behavior mirrors the prior cmd/veska contextCmd RunE.
func RunContext(ctx context.Context, p ContextParams) error {
	params := map[string]any{"symbol": p.Symbol}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	// omit repo_id so the daemon fans out by default - the common
	// cobra-CLI-plus-shared-lib pattern wants `veska context Greeter.Hello`
	// from the CLI repo to surface the library's symbol (and its cross-repo
	// edges back).
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_context_pack", params, &resp); err != nil {
		return fmt.Errorf("context: %w", err)
	}
	if p.JSONOut {
		var pretty any
		_ = json.Unmarshal(resp, &pretty)
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(pretty)
	}
	var pack contextPack
	if err := json.Unmarshal(resp, &pack); err != nil {
		return err
	}
	return renderContextPack(ctx, p.Out, pack)
}

// renderContextPack prints the text-mode context pack: header, one line per
// neighbor, then resolved cross-repo edges.
func renderContextPack(ctx context.Context, w io.Writer, pack contextPack) error {
	// a zero-node pack means the symbol didn't resolve. Say so
	// plainly + point to `veska symbol` for fuzzier lookup instead of the
	// deadpan "context for X (0 node(s))".
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
	renderContextCrossRepoEdges(ctx, w, pack)
	return nil
}

// renderContextCrossRepoEdges prints the cross-repo edges the daemon resolved
// through cross_repo_edge_stubs (). Each edge is
// labeled with the calling function/method + file:line when the graph has
// it, falling back to a per-side eng_get_node lookup.
func renderContextCrossRepoEdges(ctx context.Context, w io.Writer, pack contextPack) {
	if len(pack.CrossRepoEdges) == 0 {
		return
	}
	localByID := make(map[string]graphref.NodeInfo, len(pack.Nodes))
	for _, n := range pack.Nodes {
		if n.NodeID == "" {
			continue
		}
		localByID[n.NodeID] = graphref.NodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath}
	}
	fmt.Fprintf(w, "cross-repo edges (%d):\n", len(pack.CrossRepoEdges))
	for _, e := range pack.CrossRepoEdges {
		src, ok := localByID[e.SrcNodeID]
		if !ok || src.Name == "" {
			src = graphref.ResolveCrossRepoNode(ctx, e.SrcNodeID, "", "")
		}
		dst, ok := localByID[e.DstNodeID]
		if !ok || dst.Name == "" {
			dst = graphref.ResolveCrossRepoNode(ctx, e.DstNodeID, e.DstRepoID, e.DstBranch)
		}
		dstRepo := e.DstRepoID
		if len(dstRepo) > 12 {
			dstRepo = dstRepo[:12]
		}
		fmt.Fprintf(w, "   %s --%s--> %s in %s\n",
			graphref.FormatCrossRepoNode(src, e.SrcNodeID), e.Kind,
			graphref.FormatCrossRepoNode(dst, e.DstNodeID), dstRepo)
	}
}
