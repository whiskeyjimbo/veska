// Package clonescmd holds the delivery-layer logic behind `veska clones`
// (eng_find_clones): exact-clone detection by content_hash equality. It proxies
// to the daemon like the other read commands and renders one block per clone
// group. solov2-wfrj.
package clonescmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// clonesResp mirrors the MCP FindClonesResponse shape — enough to render the
// groups without importing the infrastructure package.
type clonesResp struct {
	Groups []struct {
		ContentHash string `json:"content_hash"`
		Size        int    `json:"size"`
		Members     []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start"`
		} `json:"members"`
	} `json:"groups"`
}

// Params bundles the inputs of Run.
type Params struct {
	RepoID  string
	Branch  string
	JSONOut bool
	Out     io.Writer
}

// Run wraps eng_find_clones: list groups of >=2 byte-identical symbols.
func Run(ctx context.Context, p Params) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}

	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_clones", params, &raw); err != nil {
		return fmt.Errorf("clones: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var resp clonesResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	render(p.Out, resp)
	return nil
}

func render(w io.Writer, resp clonesResp) {
	if len(resp.Groups) == 0 {
		fmt.Fprintln(w, "no exact clones found")
		return
	}
	for i, g := range resp.Groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%d copies (hash %s):\n", g.Size, shortHash(g.ContentHash))
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, m := range g.Members {
			fmt.Fprintf(tw, "  %s\t%s\t%s:%d\n", m.Kind, m.Name, m.FilePath, m.LineStart)
		}
		_ = tw.Flush()
	}
}

// shortHash trims a 64-char sha256 to its first 12 chars for display; the full
// hash is still available via --json.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
