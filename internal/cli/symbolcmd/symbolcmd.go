// Package symbolcmd holds the delivery-layer logic behind the `veska symbol`
// and `veska context` commands: the eng_find_symbol / eng_get_context_pack
// MCP calls, the cross-repo fallback hint, and the textual/JSON rendering of
// node lists and context packs. cmd/veska/symbol.go is reduced to Cobra
// command construction whose RunE bodies are thin calls into the Run helpers
// here (solov2-0omh.7, following the cmd = glue / logic-in-packages pattern
// from solov2-0omh.4/.5/.6).
package symbolcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// FindParams bundles the inputs of RunFind so the call stays under the
// argument-limit gate and the cmd layer can build it from flags/args.
type FindParams struct {
	Symbol  string
	RepoID  string // explicit --repo; empty means daemon fan-out
	JSONOut bool
	Out     io.Writer
	ErrOut  io.Writer
}

// RunFind wraps eng_find_symbol: it issues the lookup, emits the cross-repo
// "matches elsewhere" hint when a scoped probe comes back empty, and renders
// the node list. Behaviour mirrors the prior cmd/veska symbolCmd RunE.
func RunFind(ctx context.Context, p FindParams) error {
	params := map[string]any{"symbol": p.Symbol}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
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
		DegradedReasons []string `json:"degraded_reasons,omitempty"`
		IndexingRepos   []string `json:"indexing_repos,omitempty"`
	}
	if err := mcpclient.Call(ctx, "eng_find_symbol", params, &resp); err != nil {
		return fmt.Errorf("symbol: %w", err)
	}
	// solov2-zgwd: when the scoped probe is empty, ask every other registered
	// repo whether the symbol lives there. Non-empty in the original scope
	// short-circuits — we never re-walk the registry for a happy result.
	if len(resp.Nodes) == 0 && !p.JSONOut && p.RepoID != "" {
		PrintCrossRepoSymbolHint(ctx, p.ErrOut, p.Symbol, p.RepoID)
	}
	return RenderNodeList(p.Out, resp, p.JSONOut)
}

// PrintCrossRepoSymbolHint walks every other registered repo and prints a
// one-line hint when the symbol exists somewhere else (solov2-zgwd). Stays
// best-effort: any per-repo error is silently skipped — a stuck repo must
// not turn a successful empty result into a noisy banner. The hint only
// fires when there's at least one cross-repo match, so the "no matches
// anywhere" case is unchanged.
func PrintCrossRepoSymbolHint(ctx context.Context, errOut io.Writer, symbol, scopedRepoID string) {
	type repoView struct {
		RepoID  string `json:"repo_id"`
		ShortID string `json:"short_id"`
	}
	var lr struct {
		Repos []repoView `json:"repos"`
	}
	if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
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
		if err := mcpclient.Call(ctx, "eng_find_symbol", params, &probe); err != nil {
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

// RenderNodeList prints a {nodes:[...]} envelope (eng_find_symbol shape) as
// either pretty JSON or a greppable table. It re-marshals through a generic
// shape so it works for either {nodes} or {entries} envelopes.
func RenderNodeList(w io.Writer, resp any, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
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
			RepoID    string `json:"repo_id,omitempty"`
		} `json:"nodes"`
		DegradedReasons []string `json:"degraded_reasons,omitempty"`
		IndexingRepos   []string `json:"indexing_repos,omitempty"`
	}
	if err := json.Unmarshal(raw, &any); err != nil {
		return err
	}
	if len(any.Nodes) == 0 {
		renderNoNodeMatches(w, any.DegradedReasons, any.IndexingRepos)
		return nil
	}
	// solov2-efzv: when the daemon fanned out across repos (repo_id populated
	// on at least one hit) render a leading repo column so the user can
	// disambiguate cross-repo matches without a follow-up query.
	multiRepo := false
	for _, n := range any.Nodes {
		if n.RepoID != "" {
			multiRepo = true
			break
		}
	}
	for _, n := range any.Nodes {
		extMark := ""
		if n.External {
			extMark = " [external]"
		}
		if multiRepo {
			fmt.Fprintf(w, "%-12s %-10s %s:%d-%d  %s  (%s)%s\n",
				repocmd.ShortRepoID(n.RepoID), n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name, n.NodeID[:12], extMark)
		} else {
			fmt.Fprintf(w, "%-10s %s:%d-%d  %s  (%s)%s\n",
				n.Kind, n.FilePath, n.LineStart, n.LineEnd, n.Name, n.NodeID[:12], extMark)
		}
	}
	return nil
}

// renderNoNodeMatches prints the empty-result message plus the cold-scan
// retry hint when an active indexing window is the likely cause
// (solov2-izh6.30).
func renderNoNodeMatches(w io.Writer, degradedReasons, indexingRepos []string) {
	fmt.Fprintln(w, "no matches")
	if slices.Contains(degradedReasons, mcpinfra.DegradedReasonIndexingInProgress) {
		fmt.Fprintf(w, "  hint: %d repo(s) still indexing (%s); retry shortly or rerun the relevant `veska repo add --wait`.\n",
			len(indexingRepos), strings.Join(indexingRepos, ", "))
	}
}
