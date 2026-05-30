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
// (solov2-izh6.1).
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

// BlastParams bundles the inputs of RunBlast.
type BlastParams struct {
	Selector  string
	RepoID    string
	Direction string // raw flag value; normalized internally
	JSONOut   bool
	Out       io.Writer
}

// RunBlast wraps eng_get_blast_radius.
func RunBlast(ctx context.Context, p BlastParams) error {
	params := selectorParams(p.Selector)
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Direction != "" {
		params["direction"] = NormalizeDirection(p.Direction)
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_blast_radius", params, &resp); err != nil {
		return fmt.Errorf("blast: %w", err)
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
