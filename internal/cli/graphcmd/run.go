// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// selectorParams routes a positional selector to the node_id or symbol MCP
// param depending on whether it looks like a hex content-hash node id
func selectorParams(arg string) map[string]any {
	if LooksLikeNodeID(arg) {
		return map[string]any{"node_id": arg}
	}
	return map[string]any{"symbol": arg}
}

// CallsParams bundles the inputs of RunCalls.
type CallsParams struct {
	Selector        string
	RepoID          string
	Direction       string // raw flag value; normalized internally
	Depth           int
	ExpandCrossRepo bool
	JSONOut         bool
	Out             io.Writer
}

// RunCalls wraps eng_get_call_chain: it builds the request from the selector
// and flags, then renders the chain.
func RunCalls(ctx context.Context, p CallsParams) error {
	params := selectorParams(p.Selector)
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Direction != "" {
		params["direction"] = NormalizeDirection(p.Direction)
	}
	if p.Depth > 0 {
		params["depth"] = p.Depth
	}
	if p.ExpandCrossRepo {
		params["expand_cross_repo"] = true
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_call_chain", params, &resp); err != nil {
		return fmt.Errorf("calls: %w", err)
	}
	return RenderGraphChain(ctx, p.Out, resp, p.JSONOut)
}

// BlastMode selects which blast-radius seed RunBlast uses: a single symbol
// (default), the staged overlay (--dirty), or the working-tree diff (--diff).
// The three back onto distinct MCP tools that all return the same
// BlastResponse shape, so the renderer is shared.
type BlastMode int

const (
	BlastSymbol BlastMode = iota // seed from Selector via eng_get_blast_radius
	BlastDirty                   // staged overlay via eng_get_dirty_blast_radius
	BlastDiff                    // working-tree vs HEAD via eng_get_diff_blast_radius
)

// BlastParams bundles the inputs of RunBlast.
type BlastParams struct {
	Mode     BlastMode
	Selector string // required when Mode == BlastSymbol; ignored otherwise
	RepoID   string
	// RefA/RefB scope a BlastDiff to a git ref range (ref_a.ref_b). Both
	// empty means the working-tree-vs-HEAD default. Ignored unless
	// Mode == BlastDiff.
	RefA      string
	RefB      string
	Direction string // raw flag value; normalized internally
	JSONOut   bool
	Out       io.Writer
}

// RunBlast wraps the blast-radius tool family. The seed mode picks the tool;
// eng_get_dirty_blast_radius and eng_get_diff_blast_radius take no selector
// (the staged overlay / working-tree diff IS the seed set) and resolve the
// repo from repo_id or the connecting client's cwd, exactly like the
// single-symbol path.
func RunBlast(ctx context.Context, p BlastParams) error {
	var params map[string]any
	var tool string
	switch p.Mode {
	case BlastDirty:
		params, tool = map[string]any{}, "eng_get_dirty_blast_radius"
	case BlastDiff:
		params, tool = map[string]any{}, "eng_get_diff_blast_radius"
		// ref_a/ref_b are all-or-nothing at the tool boundary; the Cobra
		// layer guarantees both are set together (or both empty for the
		// working-tree default).
		if p.RefA != "" {
			params["ref_a"] = p.RefA
		}
		if p.RefB != "" {
			params["ref_b"] = p.RefB
		}
	default:
		params, tool = selectorParams(p.Selector), "eng_get_blast_radius"
	}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Direction != "" {
		params["direction"] = NormalizeDirection(p.Direction)
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, tool, params, &resp); err != nil {
		return fmt.Errorf("blast: %w", err)
	}
	return RenderGraphChain(ctx, p.Out, resp, p.JSONOut)
}

// NodeParams bundles the inputs of RunNode.
type NodeParams struct {
	NodeID  string
	RepoID  string
	Branch  string
	JSONOut bool
	Out     io.Writer
}

// RunNode wraps eng_get_node. The response is a GraphResponse whose {nodes}
// envelope is rendered by the shared RenderGraphChain (no edges/cross-repo on
// a single-node lookup, so it degrades to a one-row node table).
func RunNode(ctx context.Context, p NodeParams) error {
	params := map[string]any{"node_id": p.NodeID}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_node", params, &resp); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	return RenderGraphChain(ctx, p.Out, resp, p.JSONOut)
}

// FileNodesParams bundles the inputs of RunFileNodes.
type FileNodesParams struct {
	FilePath string
	RepoID   string
	Branch   string
	JSONOut  bool
	Out      io.Writer
}

// RunFileNodes wraps eng_get_file_nodes, returning every node defined in a
// single file. Like RunNode it renders the GraphResponse via RenderGraphChain.
func RunFileNodes(ctx context.Context, p FileNodesParams) error {
	params := map[string]any{"file_path": p.FilePath}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_file_nodes", params, &resp); err != nil {
		return fmt.Errorf("file-nodes: %w", err)
	}
	return RenderGraphChain(ctx, p.Out, resp, p.JSONOut)
}

// ChangedParams bundles the inputs of RunChanged. RefA/RefB are already
// resolved (positional args mapped onto the flag values by the Cobra layer).
type ChangedParams struct {
	RepoID  string
	RefA    string
	RefB    string
	JSONOut bool
	Out     io.Writer
}

// RunChanged wraps eng_find_changed_symbols, the symbol-grain diff between two
// git refs.
func RunChanged(ctx context.Context, p ChangedParams) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.RefA != "" {
		params["ref_a"] = p.RefA
	}
	if p.RefB != "" {
		params["ref_b"] = p.RefB
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_changed_symbols", params, &resp); err != nil {
		return fmt.Errorf("changed: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(resp, &pretty)
		return enc.Encode(pretty)
	}
	return RenderChangedSymbols(p.Out, resp)
}
