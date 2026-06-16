// Package similarcmd holds the delivery-layer logic behind `veska similar`
// (eng_search_similar) and `veska related` (eng_find_related). Both ride the
// same vector-neighbourhood path on the daemon and return a SearchResponse
// envelope, so they share one renderer here.
package similarcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// searchResp is the local mirror of the MCP SearchResponse shape — enough of
// it to render a neighbour table without importing the infrastructure package.
type searchResp struct {
	Results []struct {
		NodeID    string  `json:"node_id"`
		Name      string  `json:"name"`
		Kind      string  `json:"kind"`
		FilePath  string  `json:"file_path"`
		LineStart int     `json:"line_start"`
		Score     float32 `json:"score"`
		RepoID    string  `json:"repo_id"`
	} `json:"results"`
	DegradedReasons []string `json:"degraded_reasons"`
}

// SimilarParams bundles the inputs of RunSimilar.
type SimilarParams struct {
	Selector string // symbol name or node_id; routed like calls/blast
	RepoID   string
	K        int
	JSONOut  bool
	Out      io.Writer
}

// RunSimilar wraps eng_search_similar: vector-nearest-neighbour search seeded
// by an existing symbol or node_id.
func RunSimilar(ctx context.Context, p SimilarParams) error {
	params := map[string]any{}
	if graphcmd.LooksLikeNodeID(p.Selector) {
		params["node_id"] = p.Selector
	} else {
		params["symbol"] = p.Selector
	}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.K > 0 {
		params["k"] = p.K
	}
	return callAndRender(ctx, "eng_search_similar", params, p.Out, p.JSONOut)
}

// RelatedParams bundles the inputs of RunRelated.
type RelatedParams struct {
	FilePath string
	Line     int
	RepoID   string
	K        int
	JSONOut  bool
	Out      io.Writer
}

// RunRelated wraps eng_find_related: find symbols similar to the code at a
// (file_path, line) anchor. The daemon resolves the smallest enclosing node
// and reuses the eng_search_similar neighbourhood path.
func RunRelated(ctx context.Context, p RelatedParams) error {
	params := map[string]any{"file_path": p.FilePath, "line": p.Line}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.K > 0 {
		params["k"] = p.K
	}
	return callAndRender(ctx, "eng_find_related", params, p.Out, p.JSONOut)
}

func callAndRender(ctx context.Context, tool string, params map[string]any, out io.Writer, jsonOut bool) error {
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, tool, params, &raw); err != nil {
		return fmt.Errorf("%s: %w", shortTool(tool), err)
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var resp searchResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	renderNeighbours(out, resp)
	return nil
}

func renderNeighbours(w io.Writer, resp searchResp) {
	if len(resp.Results) == 0 {
		fmt.Fprintln(w, "no similar symbols found")
		for _, d := range resp.DegradedReasons {
			fmt.Fprintf(w, "[degraded: %s]\n", d)
		}
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SCORE\tKIND\tNAME\tLOCATION")
	for _, r := range resp.Results {
		loc := fmt.Sprintf("%s:%d", r.FilePath, r.LineStart)
		fmt.Fprintf(tw, "%.4f\t%s\t%s\t%s\n", r.Score, r.Kind, r.Name, loc)
	}
	_ = tw.Flush()
	for _, d := range resp.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
}

// shortTool turns "eng_search_similar"/"eng_find_related" into the user-facing
// verb for error prefixes.
func shortTool(tool string) string {
	switch tool {
	case "eng_search_similar":
		return "similar"
	case "eng_find_related":
		return "related"
	}
	return tool
}
