// Package ownercmd holds the delivery-layer logic behind `veska owner`
// (eng_find_owner): resolve the owner of a file via CODEOWNERS (longest-match)
// or a git-blame fallback. The anchor may be a path, a symbol, or a node_id —
// the latter two resolve to their defining file first. solov2-yh5a.
package ownercmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// Params bundles the inputs of Run. Anchor is the positional path/symbol/
// node_id; RepoID is required by eng_find_owner and resolved by the caller
// (cmd/veska) before this point.
type Params struct {
	Anchor  string
	RepoID  string
	Branch  string
	JSONOut bool
	Out     io.Writer
}

// ownerResp mirrors the {owner, source} shape eng_find_owner returns.
type ownerResp struct {
	Owner  string `json:"owner"`
	Source string `json:"source"`
}

// Run wraps eng_find_owner. The anchor is routed to the matching MCP param:
// a hex node_id → node_id, a path-shaped token → file_path, otherwise symbol.
func Run(ctx context.Context, p Params) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	switch {
	case graphcmd.LooksLikeNodeID(p.Anchor):
		params["node_id"] = p.Anchor
	case looksLikePath(p.Anchor):
		params["file_path"] = p.Anchor
	default:
		params["symbol"] = p.Anchor
	}
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_owner", params, &raw); err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var resp ownerResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if resp.Owner == "" {
		fmt.Fprintln(p.Out, "no owner found")
		return nil
	}
	fmt.Fprintf(p.Out, "%s (via %s)\n", resp.Owner, resp.Source)
	return nil
}

// looksLikePath reports whether s reads as a file path rather than a symbol
// name: it contains a path separator or ends in a source extension. A bare
// "." is NOT enough — qualified Go symbols like "FlagSet.Parse" or
// "pkg.Func" contain a dot but are symbols, so anchoring on ".go"/".ts"/etc.
// (or a slash) keeps them on the symbol branch (solov2-yh5a).
func looksLikePath(s string) bool {
	if strings.Contains(s, "/") {
		return true
	}
	for _, ext := range []string{".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java"} {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}
