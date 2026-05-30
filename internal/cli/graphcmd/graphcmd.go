// Package graphcmd holds the delivery-layer logic behind the `veska calls`,
// `veska blast`, and `veska changed` commands: the eng_get_call_chain /
// eng_get_blast_radius / eng_find_changed_symbols MCP calls and the
// textual/JSON rendering of their {nodes, edges, cross_repo_edges} and
// added/removed/modified envelopes. cmd/veska/graph.go is reduced to Cobra
// command construction whose RunE bodies are thin calls into the Run helpers
// here (solov2-0omh.7, following the cmd = glue / logic-in-packages pattern
// from solov2-0omh.4/.5/.6).
package graphcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/graphref"
)

// NormalizeDirection accepts the user-friendly aliases callers/callees (which
// match the help-text prose) in addition to the canonical in/out enum the
// daemon expects. solov2-5out.
func NormalizeDirection(d string) string {
	switch d {
	case "callers":
		return "in"
	case "callees":
		return "out"
	}
	return d
}

// LooksLikeNodeID reports whether s looks like a hex content-hash node ID.
// The threshold is 12 hex chars — the width of the display short_id the CLI
// prints — so a user copy-pasting the "(66f083714906)" suffix from `veska
// symbol` output into `veska calls`/`veska blast` is routed through the
// node_id path and prefix-expanded daemon-side, instead of being misread as
// a symbol name (solov2-izh6.1).
func LooksLikeNodeID(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// CrossRepoEdgeDTO mirrors the daemon's CrossRepoEdge JSON shape locally so
// RenderGraphChain can decode without importing the MCP package.
type CrossRepoEdgeDTO struct {
	SrcNodeID string `json:"src_node_id"`
	DstNodeID string `json:"dst_node_id"`
	DstRepoID string `json:"dst_repo_id"`
	DstBranch string `json:"dst_branch"`
	Kind      string `json:"kind"`
	// SrcLine is the 1-indexed line of the originating call_expression. When
	// set, the renderer uses it instead of the caller node's declaration line
	// so each cross-repo edge points to its actual call site (solov2-izh6.31).
	// 0 = unknown (pre-migration data).
	SrcLine int `json:"src_line,omitempty"`
}

// graphChainEnv is the {nodes, edges, cross_repo_edges} envelope returned by
// eng_get_call_chain / eng_get_blast_radius.
type graphChainEnv struct {
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
	IndexingRepos   []string           `json:"indexing_repos,omitempty"`
}

// RenderGraphChain prints a {nodes, edges, cross_repo_edges} envelope as a
// greppable table. Used by `veska calls` and `veska blast`. ctx feeds the
// eng_get_node lookups that resolve cross-repo edge endpoints from opaque
// hex to "symbol in file:line" form (solov2-y59h).
func RenderGraphChain(ctx context.Context, w io.Writer, raw json.RawMessage, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var env graphChainEnv
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if len(env.Nodes) == 0 && len(env.CrossRepoEdges) == 0 {
		renderEmptyChain(w, env)
		return nil
	}
	// Build a local lookup so cross-repo edges whose src/dst is already in the
	// response envelope can be resolved without a round-trip per side.
	localByID := make(map[string]graphref.NodeInfo, len(env.Nodes))
	for _, n := range env.Nodes {
		if n.NodeID == "" {
			continue
		}
		localByID[n.NodeID] = graphref.NodeInfo{Name: n.Name, Kind: n.Kind, FilePath: n.FilePath, Line: n.LineStart}
		fmt.Fprintf(w, "%-10s %s:%d-%d  %s\n", n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name)
	}
	renderChainCrossRepoEdges(ctx, w, env, localByID)
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
	return nil
}

// renderEmptyChain prints the "no nodes in chain" message plus per-reason
// hints for the degraded reasons that need decoding (solov2-izh6.30,
// solov2-4soa, solov2-izh6.22).
func renderEmptyChain(w io.Writer, env graphChainEnv) {
	fmt.Fprintln(w, "no nodes in chain")
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
		switch d {
		case "indexing_in_progress":
			fmt.Fprintf(w, "  hint: %d repo(s) still indexing (%s); retry shortly or rerun with `veska repo add --wait` to block until ready.\n",
				len(env.IndexingRepos), strings.Join(env.IndexingRepos, ", "))
		case "chained_selectors_unresolved":
			fmt.Fprintln(w, "  hint: parser can't resolve chained selector expressions (e.g. rootCmd.AddCommand(...).Execute()),")
			fmt.Fprintln(w, "        so call edges from cobra-style top-level var initialisers are attributed to the package node.")
			fmt.Fprintln(w, "        try `veska blast <symbol>` or `veska context <symbol>` for a graph-wide view.")
		case "external_callees_only":
			fmt.Fprintln(w, "  hint: this symbol only calls into stdlib or unregistered modules, so there are no")
			fmt.Fprintln(w, "        edges to follow in the indexed graph. try `veska blast <symbol>` for callers,")
			fmt.Fprintln(w, "        or register the dependency repo so its symbols become first-class nodes.")
		}
	}
}

// renderChainCrossRepoEdges prints the cross-repo edges block, resolving each
// endpoint to "symbol in file:line" form and emitting the package-grain note
// when any src landed on a package node (solov2-urqy).
func renderChainCrossRepoEdges(ctx context.Context, w io.Writer, env graphChainEnv, localByID map[string]graphref.NodeInfo) {
	if len(env.CrossRepoEdges) == 0 {
		return
	}
	fmt.Fprintf(w, "cross-repo edges (%d):\n", len(env.CrossRepoEdges))
	anyPackageSrc := false
	for _, e := range env.CrossRepoEdges {
		src, ok := localByID[e.SrcNodeID]
		if !ok || src.Name == "" {
			src = graphref.ResolveCrossRepoNode(ctx, e.SrcNodeID, "", "")
		}
		dst, ok := localByID[e.DstNodeID]
		if !ok || dst.Name == "" {
			dst = graphref.ResolveCrossRepoNode(ctx, e.DstNodeID, e.DstRepoID, e.DstBranch)
		}
		if src.Kind == "package" {
			anyPackageSrc = true
		}
		// solov2-izh6.31: prefer the edge's own SrcLine over the caller node's
		// declaration line so a function with multiple cross-repo calls
		// renders each at its actual call site.
		if e.SrcLine > 0 {
			src.Line = e.SrcLine
		}
		fmt.Fprintf(w, "  %s --%s--> %s in %s\n",
			graphref.FormatCrossRepoNode(src, e.SrcNodeID), e.Kind,
			graphref.FormatCrossRepoNode(dst, e.DstNodeID), shortID(e.DstRepoID))
	}
	if anyPackageSrc {
		// solov2-urqy: cross-repo CALLS landing on a `package` src usually
		// means the call lives inside an anonymous function in a top-level var
		// initialiser (e.g. cobra's `Run: func(...){...}`).
		fmt.Fprintln(w, "  note: package-grain src means the caller is an anonymous func in a top-level var initialiser (e.g. cobra Run/RunE).")
		fmt.Fprintln(w, "        run `veska context <caller-symbol>` or grep that file for the dst symbol to pinpoint the call site.")
	}
}

// shortID slices a content-hashed id to its 12-char display form, leaving
// shorter inputs untouched.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
