// SPDX-License-Identifier: AGPL-3.0-only

// Package clonescmd holds the delivery-layer logic behind `veska clones`
// (eng_find_duplicates seed=clones): exact-clone detection by content_hash equality. It proxies
// to the daemon like the other read commands and renders one block per clone
// group.
package clonescmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// cloneMember mirrors one wire member shared by exact groups and near clusters.
type cloneMember struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start"`
}

// clonesResp mirrors the MCP FindClonesResponse shape - enough to render
// either exact groups or near clusters without importing the infra package.
type clonesResp struct {
	Mode   string `json:"mode"`
	Groups []struct {
		ContentHash string        `json:"content_hash"`
		Size        int           `json:"size"`
		Members     []cloneMember `json:"members"`
	} `json:"groups"`
	Clusters []struct {
		Size     int           `json:"size"`
		MinScore float32       `json:"min_score"`
		MaxScore float32       `json:"max_score"`
		Members  []cloneMember `json:"members"`
	} `json:"clusters"`
}

// Params bundles the inputs of Run.
type Params struct {
	RepoID   string
	Branch   string
	Near     bool
	MinScore float64
	JSONOut  bool
	Out      io.Writer
}

// Run wraps eng_find_duplicates seed=clones: exact byte-identical groups
// (default) or, with Near set, fuzzy near-duplicate clusters from thresholded
// SIMILAR_TO edges.
func Run(ctx context.Context, p Params) error {
	params := map[string]any{"seed": "clones"}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	if p.Near {
		params["mode"] = "near"
		if p.MinScore > 0 {
			params["min_score"] = p.MinScore
		}
	}

	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_duplicates", params, &raw); err != nil {
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
	if resp.Mode == "near" {
		renderClusters(w, resp)
		return
	}
	renderGroups(w, resp)
}

func renderGroups(w io.Writer, resp clonesResp) {
	if len(resp.Groups) == 0 {
		fmt.Fprintln(w, "no exact clones found")
		return
	}
	for i, g := range resp.Groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%d copies (hash %s):\n", g.Size, shortHash(g.ContentHash))
		renderMembers(w, g.Members)
	}
}

func renderClusters(w io.Writer, resp clonesResp) {
	if len(resp.Clusters) == 0 {
		fmt.Fprintln(w, "no near-duplicate clusters found")
		fmt.Fprintln(w, "(near mode needs SIMILAR_TO edges with scores - reindex the repo if it was promoted before scoring landed)")
		return
	}
	for i, c := range resp.Clusters {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%d similar (score %.3f–%.3f):\n", c.Size, c.MinScore, c.MaxScore)
		renderMembers(w, c.Members)
	}
}

func renderMembers(w io.Writer, members []cloneMember) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, m := range members {
		fmt.Fprintf(tw, "  %s\t%s\t%s:%d\n", m.Kind, m.Name, m.FilePath, m.LineStart)
	}
	_ = tw.Flush()
}

// shortHash trims a 64-char sha256 to its first 12 chars for display; the full
// hash is still available via --json.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
