// SPDX-License-Identifier: AGPL-3.0-only

package searchcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/search"
)

// SearchHitView is the CLI's wire shape for one hit. It mirrors the MCP
// eng_search_semantic node DTO (snake_case) so `veska search --json` and the
// tool emit byte-identical envelopes (, AC3). RepoID is populated
// only by the cross-repo fanout; single-repo search omits it so
// JSON output stays byte-identical with the daemon's.
type SearchHitView struct {
	NodeID          string  `json:"node_id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"`
	FilePath        string  `json:"file_path"`
	LineStart       int     `json:"line_start,omitempty"`
	LineEnd         int     `json:"line_end,omitempty"`
	Score           float32 `json:"score"`
	ScoreNormalized float32 `json:"score_normalized"`
	Tier            string  `json:"tier,omitempty"`
	Snippet         string  `json:"snippet,omitempty"`
	RepoID          string  `json:"repo_id,omitempty"`
}

// SearchEnvelope is the {results, degraded_reasons} wrapper shared by the
// daemon-dial path (decoded from eng_search_semantic) and the in-process
// fallback (mapped from search.Response).
type SearchEnvelope struct {
	Results         []SearchHitView `json:"results"`
	DegradedReasons []string        `json:"degraded_reasons,omitempty"`
}

// RenderSearchResults maps an in-process search.Response into the wire
// envelope and renders it.
func RenderSearchResults(w io.Writer, resp search.Response, jsonOut bool) error {
	env := buildSearchEnvelope(resp)
	return RenderSearchEnvelope(w, env, jsonOut)
}

// buildSearchEnvelope maps an in-process search.Response into the wire
// envelope used by both render paths. Extracted so the search flow can post
// process the envelope (e.g. shortening ephemeral cache-tier paths) before
// rendering.
func buildSearchEnvelope(resp search.Response) SearchEnvelope {
	env := SearchEnvelope{DegradedReasons: resp.DegradedReasons}
	env.Results = make([]SearchHitView, 0, len(resp.Results))
	for _, r := range resp.Results {
		env.Results = append(env.Results, SearchHitView{
			NodeID:    r.NodeID,
			Name:      r.SymbolPath,
			Kind:      r.Kind,
			FilePath:  r.FilePath,
			LineStart: r.LineStart,
			LineEnd:   r.LineEnd,
			Score:     r.Score,
			Snippet:   r.Snippet,
		})
	}
	return env
}

// ephemeralDisplayName picks a short, human-readable name for an ephemeral
// cache-tier repo: the last path segment of canonical_url (e.g. "pflag" from
// "https://github.com/spf13/pflag.git"), falling back to a 12-char prefix of
// repoID.
func ephemeralDisplayName(canonicalURL, repoID string) string {
	if canonicalURL != "" {
		base := canonicalURL
		base = strings.TrimSuffix(base, ".git")
		base = strings.TrimRight(base, "/")
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if base != "" {
			return base
		}
	}
	if len(repoID) > 12 {
		return repoID[:12]
	}
	return repoID
}

// prettifyEphemeralPaths rewrites file_path on every hit whose path lives
// under the ephemeral cache repo dir, replacing the cache prefix with
// "<displayName>/" so the user sees `pflag/flag.go:194-206` rather than the
// unscannable 64-char sha. JSON output should skip this - callers gate on
// jsonOut.
func prettifyEphemeralPaths(env *SearchEnvelope, cacheRepoDir, displayName string) {
	if env == nil || cacheRepoDir == "" || displayName == "" {
		return
	}
	prefix := cacheRepoDir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	for i := range env.Results {
		fp := env.Results[i].FilePath
		if strings.HasPrefix(fp, prefix) {
			env.Results[i].FilePath = displayName + "/" + fp[len(prefix):]
		}
	}
}

// RenderSearchEnvelope emits the envelope as indented JSON (--json) or a
// greppable one-line-per-hit table. Results is always a non-nil slice so the
// JSON carries "results": on a miss.
func RenderSearchEnvelope(w io.Writer, env SearchEnvelope, jsonOut bool) error {
	if env.Results == nil {
		env.Results = []SearchHitView{}
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	}
	if len(env.Results) == 0 {
		renderEmptyResults(w, env)
		return nil
	}
	renderResultTable(w, env)
	return nil
}

// renderEmptyResults handles the zero-hit text path. A silent miss reads as
// broken to a new user: hint at warming embeddings when we can
// see the daemon's pending count, otherwise print a plain "no results" so the
// command never exits without any text feedback.
func renderEmptyResults(w io.Writer, env SearchEnvelope) {
	if pending, ok := pendingEmbedsHint(); ok && pending > 0 {
		fmt.Fprintf(w, "no results (%d embeds pending - try again shortly)\n", pending)
	} else {
		fmt.Fprintln(w, "no results")
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
}

// renderResultTable renders the one-line-per-hit table for a non-empty
// envelope, including the column legend, optional multi-repo column, weak-top
// note, and degraded-reason footer.
func renderResultTable(w io.Writer, env SearchEnvelope) {
	// tier + score_normalized arrive on the DTO so the CLI
	// doesn't recompute them. We still need the absolute top for the
	// weak-top hint below.
	var top float32
	for _, r := range env.Results {
		if r.Score > top {
			top = r.Score
		}
	}
	// when the fanout populated RepoID, render it as a leading
	// column so the user can disambiguate hits across repos.
	multiRepo := false
	for _, r := range env.Results {
		if r.RepoID != "" {
			multiRepo = true
			break
		}
	}
	// one-line legend so the score/norm/tier columns are not
	// inscrutable to first-time users. The numbers are post-fusion RRF
	//  meaningful only relative to other hits in this query,
	// not as absolute confidence.
	fmt.Fprintln(w, "# tier: top|strong|weak (relative to this query); score: post-fusion RRF; norm: score / top_hit_score")
	for _, r := range env.Results {
		renderResultRow(w, r, top, multiRepo)
	}
	// tiers are relative to this query's top hit - a query with
	// no strong absolute match still gets a "top" label, which reads as
	// confidence the data can't back up. Below an absolute floor, append a
	// one-liner so the user knows the labels are relative and recall is weak.
	if top > 0 && top < weakTopAbsolute {
		fmt.Fprintf(w, "note: top match score is low (%.4f) - labels are relative to this query; recall may be weak. Try refining the query.\n", top)
	}
	for _, d := range env.DegradedReasons {
		fmt.Fprintf(w, "[degraded: %s%s]\n", d, degradedReasonHint(d))
	}
}

// renderResultRow renders a single hit line, computing the fallback tier for
// the in-process path that may set Score but not Tier yet.
func renderResultRow(w io.Writer, r SearchHitView, top float32, multiRepo bool) {
	tier := r.Tier
	if tier == "" {
		// Fallback for the in-process search path. Computed in-line matches
		// the application-layer logic 1:1.
		tier = "weak"
		if top > 0 && r.Score/top >= 0.95 {
			tier = "top"
		} else if top > 0 && r.Score/top >= 0.80 {
			tier = "strong"
		}
	}
	if multiRepo {
		short := r.RepoID
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(w, "%-12s %-8s %s:%d-%d  %s  (%s, score=%.4f norm=%.2f)\n",
			short, r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name, tier, r.Score, r.ScoreNormalized)
		return
	}
	fmt.Fprintf(w, "%-8s %s:%d-%d  %s  (%s, score=%.4f norm=%.2f)\n",
		r.Kind, r.FilePath, r.LineStart, r.LineEnd, r.Name, tier, r.Score, r.ScoreNormalized)
}

// weakTopAbsolute is the absolute-score floor below which a query's top hit is
// considered weak. Score is post-fusion RRF:
//
//	rank-1 in one list only → 1/(60+1) = 0.01639
//	rank-1 in both lists → 2 * 1/(60+1) = 0.03279
//
// A top below ~0.018 means even the best hit only made it into ONE retriever's
// list - the cross-corroboration signal is missing and recall is weak. The
// floor sits a hair above 0.0164 so a single-list rank-1 (the common small
// corpus case) trips the hint and prompts the user to refine.
const weakTopAbsolute = 0.018

// degradedReasonHint maps an in-band degraded_reasons code to a one-line
// actionable hint appended to the rendered line. Empty when no hint applies,
// so the bare code is still printed. Hints are deliberately
// short so the table layout stays readable.
func degradedReasonHint(code string) string {
	switch code {
	case "embeddings_pending":
		if pending, ok := pendingEmbedsHint(); ok && pending > 0 {
			return fmt.Sprintf(" - ~%d embeds still queued; re-run shortly for fuller recall", pending)
		}
		return " - embedder worker is still draining; re-run shortly for fuller recall"
	case "low_quality_static_embedder":
		return " - install the model2vec weights for better recall: `veska install model2vec`"
	case "no_post_registration_commits":
		return " - only populates after commits land while the repo is registered with veska"
	default:
		return ""
	}
}
