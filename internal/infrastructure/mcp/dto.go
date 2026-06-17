// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// This file defines the unified snake_case wire contract (JSON DTOs) for MCP tools.
// Domain entities and application projections carry no JSON tags so the core remains isolated from the wire format.
// All node-returning tools map their results to nodeDTO to provide a consistent schema for agents.

// nodeDTO is the canonical node shape returned by all graph, search, and blast tools.
// RepoID is included only in cross-repo fanout responses to disambiguate hits.
type nodeDTO struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Summary is a short natural-language description: the LLM summary lane's
	// stored value when present, else a heuristic computed from the node. Always
	// populated so the field is a stable part of the default projection.
	Summary  string `json:"summary,omitempty"`
	Language string `json:"language,omitempty"`
	Exported *bool  `json:"exported,omitempty"`
	RepoID   string `json:"repo_id,omitempty"`
	// External indicates if a symbol resides in a vendored or module-cache dependency.
	// Omitted on first-party files to keep the wire payload stable.
	External bool `json:"external,omitempty"`
}

type edgeDTO struct {
	EdgeID     string `json:"edge_id"`
	SrcNodeID  string `json:"src_node_id"`
	DstNodeID  string `json:"dst_node_id"`
	Kind       string `json:"kind"`
	Confidence int    `json:"confidence"`
	Resolved   bool   `json:"resolved"`
	SourceLine *int   `json:"source_line,omitempty"`
}

// searchHitDTO extends nodeDTO with search score and snippet metadata.
// Score represents the post-fusion Reciprocal Rank Fusion (RRF) score rather than a similarity value,
// which is the only reliable signal for comparing hits within a query.
type searchHitDTO struct {
	nodeDTO
	Score   float32 `json:"score"`
	Snippet string  `json:"snippet,omitempty"`
	// ScoreNormalized is rescaled to a [0, 1] range to let agents threshold results
	// without needing to interpret raw RRF scores.
	ScoreNormalized float32 `json:"score_normalized"`
	// Tier categorizes the result confidence relative to the query's best hit (e.g., 'top', 'strong', 'weak').
	Tier string `json:"tier,omitempty"`
}

type blastEntryDTO struct {
	nodeDTO
	Distance int    `json:"distance"`
	Snippet  string `json:"snippet,omitempty"`
	// IsHub indicates that a node's neighbor count exceeded the hub threshold during BFS, halting further expansion.
	IsHub bool `json:"is_hub,omitempty"`
	// Pending is true if the node is in the edges table but has not yet been hydrated in the nodes table during a concurrent reparse.
	Pending bool `json:"pending,omitempty"`
}

func nodeToDTO(n *domain.Node) nodeDTO {
	if n == nil {
		return nodeDTO{}
	}
	d := nodeDTO{
		NodeID:   string(n.ID),
		Name:     n.Name,
		Kind:     string(n.Kind),
		FilePath: n.Path,
		Exported: n.Exported,
	}
	if n.External != nil && *n.External {
		d.External = true
	}
	if n.Lines != nil {
		d.LineStart = n.Lines.Start
		d.LineEnd = n.Lines.End
	}
	if n.Signature != nil {
		d.Signature = *n.Signature
	}
	// Summary: the stored LLM summary when the summary lane has run, else the
	// heuristic, so the contract field is always populated.
	if n.ShortSummary != nil {
		d.Summary = *n.ShortSummary
	} else {
		d.Summary = n.HeuristicSummary()
	}
	if n.Language != nil {
		d.Language = *n.Language
	}
	return d
}

// nodesToDTO maps domain nodes to DTOs, filtering out internal chunk pseudo-nodes to avoid exposing implementation-level segment fragments to the client.
func nodesToDTO(in []*domain.Node) []nodeDTO {
	out := make([]nodeDTO, 0, len(in))
	for _, n := range in {
		if n != nil && n.Kind == domain.KindChunk {
			continue
		}
		out = append(out, nodeToDTO(n))
	}
	return out
}

func edgeToDTO(e *domain.Edge) edgeDTO {
	d := edgeDTO{
		EdgeID:     e.ID,
		SrcNodeID:  string(e.Src),
		DstNodeID:  string(e.Tgt),
		Kind:       string(e.Kind),
		Confidence: int(e.Confidence),
		Resolved:   e.Resolved,
		SourceLine: e.SourceLine,
	}
	return d
}

func edgesToDTO(in []*domain.Edge) []edgeDTO {
	out := make([]edgeDTO, 0, len(in))
	for _, e := range in {
		out = append(out, edgeToDTO(e))
	}
	return out
}

func searchResultToDTO(r search.Result) searchHitDTO {
	return searchHitDTO{
		nodeDTO: nodeDTO{
			NodeID:    r.NodeID,
			Name:      r.SymbolPath,
			Kind:      r.Kind,
			FilePath:  r.FilePath,
			LineStart: r.LineStart,
			LineEnd:   r.LineEnd,
		},
		Score:   r.Score,
		Snippet: r.Snippet,
	}
}

func searchResultsToDTO(in []search.Result) []searchHitDTO {
	// Chunks are excluded from score normalization and tier calculation to prevent distorting the scale.
	visible := make([]search.Result, 0, len(in))
	for _, r := range in {
		if r.Kind == string(domain.KindChunk) {
			continue
		}
		visible = append(visible, r)
	}
	normalized := search.NormalizeScores(visible)
	var topScore float32
	for _, r := range visible {
		if r.Score > topScore {
			topScore = r.Score
		}
	}
	out := make([]searchHitDTO, 0, len(visible))
	for i, r := range visible {
		d := searchResultToDTO(r)
		d.ScoreNormalized = normalized[i]
		d.Tier = search.ScoreTier(r.Score, topScore)
		out = append(out, d)
	}
	return out
}

func blastEntryToDTO(e blastradius.Entry) blastEntryDTO {
	return blastEntryDTO{
		nodeDTO: nodeDTO{
			NodeID:    e.NodeID,
			Name:      e.SymbolPath,
			Kind:      e.Kind,
			FilePath:  e.FilePath,
			LineStart: e.LineStart,
			LineEnd:   e.LineEnd,
		},
		Distance: e.Distance,
		Snippet:  e.Snippet,
		IsHub:    e.IsHub,
		Pending:  e.Pending,
	}
}

func blastEntriesToDTO(in []blastradius.Entry) []blastEntryDTO {
	out := make([]blastEntryDTO, 0, len(in))
	for _, e := range in {
		// Filter out chunk nodes to avoid exposing distance metrics for internal file fragments.
		if e.Kind == string(domain.KindChunk) {
			continue
		}
		out = append(out, blastEntryToDTO(e))
	}
	return out
}
