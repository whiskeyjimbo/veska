// Package duplicatescmd holds the delivery-layer logic behind `veska duplicates`
// (eng_find_clusters): the unified, tier-labeled similar-code view (exact +
// structural + near) for de-dupe triage. It proxies to the daemon like the other
// read commands and renders one block per cluster, grouped tightest tier first.
package duplicatescmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

type clusterMember struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	RepoID    string `json:"repo_id"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
}

// clustersResp mirrors the MCP FindClustersResponse shape - enough to render
// without importing the infra package.
type clustersResp struct {
	Scope    string `json:"scope"`
	Clusters []struct {
		Tier      string          `json:"tier"`
		Size      int             `json:"size"`
		Score     float32         `json:"score"`
		CrossRepo bool            `json:"cross_repo"`
		Members   []clusterMember `json:"members"`
	} `json:"clusters"`
}

// Params bundles the inputs of Run.
type Params struct {
	RepoID   string
	Branch   string
	AllRepos bool
	Tiers    string
	Path     string
	MinScore float64
	JSONOut  bool
	Out      io.Writer
}

// Run wraps eng_find_clusters: ranked exact/structural/near clusters over one
// repo, or across all registered repos with AllRepos.
func Run(ctx context.Context, p Params) error {
	params := map[string]any{}
	if p.AllRepos {
		params["scope"] = "all"
	} else if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	if p.Tiers != "" {
		params["tiers"] = p.Tiers
	}
	if p.Path != "" {
		params["path"] = p.Path
	}
	if p.MinScore > 0 {
		params["min_score"] = p.MinScore
	}

	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_clusters", params, &raw); err != nil {
		return fmt.Errorf("duplicates: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var resp clustersResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	render(p.Out, resp)
	return nil
}

func render(w io.Writer, resp clustersResp) {
	if len(resp.Clusters) == 0 {
		fmt.Fprintln(w, "no similar-code clusters found")
		fmt.Fprintln(w, "(structural/near tiers need structural_hash + scored SIMILAR_TO edges - reindex a graph promoted before they landed)")
		return
	}
	for i, c := range resp.Clusters {
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := fmt.Sprintf("[%s] %d members", c.Tier, c.Size)
		if c.Tier == "near" {
			head += fmt.Sprintf(" (score %.3f)", c.Score)
		}
		if c.CrossRepo {
			head += " (cross-repo)"
		}
		fmt.Fprintln(w, head+":")
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, m := range c.Members {
			loc := fmt.Sprintf("%s:%d", m.FilePath, m.LineStart)
			if c.CrossRepo {
				loc = shortRepo(m.RepoID) + " " + loc
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", m.Kind, m.Name, loc)
		}
		_ = tw.Flush()
	}
}

// shortRepo trims a 64-char repo id to 12 chars for cross-repo display.
func shortRepo(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
