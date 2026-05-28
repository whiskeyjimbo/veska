package mcp

import (
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// This file owns the single snake_case wire contract for nodes and edges
// across every MCP tool (solov2-elt). Domain entities (domain.Node,
// domain.Edge) and the narrow application projections (search.Result,
// blastradius.Entry) deliberately carry no json tags — the hexagonal core
// stays ignorant of the wire format — so the MCP adapter owns the mapping.
// Every node-returning tool projects through nodeDTO so an agent can parse
// one shape regardless of which tool produced it. Always-null internal
// fields (raw_content, content_hash) are intentionally dropped.

// nodeDTO is the canonical node shape returned by all graph/search/blast
// tools. Name carries the fully-qualified symbol name (e.g. "Server.Start").
// RepoID is populated only on responses from a cross-repo fanout (solov2-g8fh)
// so callers can disambiguate hits when the query spans repos; single-repo
// responses omit it to keep the wire shape byte-stable.
type nodeDTO struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"file_path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Signature string `json:"signature,omitempty"`
	Language  string `json:"language,omitempty"`
	Exported  *bool  `json:"exported,omitempty"`
	RepoID    string `json:"repo_id,omitempty"`
	// External marks hits from a registered repo's vendored or
	// module-cache dependency (solov2-bchl). Agents inspecting a hit
	// can use this to decide "is this our code or someone else's?"
	// without parsing file_path. Omitted on first-party rows so the
	// wire shape stays byte-stable for callers that don't care.
	External bool `json:"external,omitempty"`
}

// edgeDTO is the canonical edge shape returned by eng_get_call_chain.
type edgeDTO struct {
	EdgeID     string `json:"edge_id"`
	SrcNodeID  string `json:"src_node_id"`
	DstNodeID  string `json:"dst_node_id"`
	Kind       string `json:"kind"`
	Confidence int    `json:"confidence"`
	Resolved   bool   `json:"resolved"`
	SourceLine *int   `json:"source_line,omitempty"`
}

// searchHitDTO is a node plus its retrieval score and inline snippet.
//
// Score is the post-fusion RRF score, NOT a similarity. Hybrid search runs
// vector + lexical retrieval and fuses with Reciprocal Rank Fusion (RRF),
// summing 1/(60+rank) across both lists. A single-list rank-1 hit scores
// ~0.0164; the strongest possible hit (rank 1 in both lists) is ~0.0328.
// The raw vector similarity (1/(1+L2dist)) is therefore NOT exposed on
// this field — RRF is the only signal that's comparable across hits in
// the same query. Compare hits relative to each other; absolute values
// don't map onto a 0..1 similarity scale (solov2-vee5).
type searchHitDTO struct {
	nodeDTO
	Score   float32 `json:"score"`
	Snippet string  `json:"snippet,omitempty"`
	// ScoreNormalized is the min-max-rescaled score (0..1) within
	// this single response's result set. Provides a calibrated number
	// agents can threshold on without interpreting raw RRF — which
	// clusters in 0.016–0.033 and reads as low-confidence even when
	// the hit is the strongest available (solov2-hl70). Use Score for
	// reproducible cross-query comparisons; use ScoreNormalized for
	// intra-query ranking decisions.
	ScoreNormalized float32 `json:"score_normalized"`
	// Tier is the post-rerank confidence band relative to this query's
	// top hit. "top" / "strong" / "weak"; ScoreTier in
	// internal/application/search is the source of truth. Pair with
	// the "weak top absolute" hint on degraded_reasons to detect the
	// case where every result is weak in absolute terms (solov2-hl70).
	Tier string `json:"tier,omitempty"`
}

// blastEntryDTO is a node plus its BFS distance from the nearest seed.
type blastEntryDTO struct {
	nodeDTO
	Distance int    `json:"distance"`
	Snippet  string `json:"snippet,omitempty"`
	// IsHub flags nodes whose neighbour count exceeded the hub-degree
	// threshold during BFS — BFS reported the node but did NOT expand
	// through it, so the entry is informational (the framework registry
	// node itself) rather than the start of a chain (solov2-l2f5).
	IsHub bool `json:"is_hub,omitempty"`
	// Pending is true when NodeLookup couldn't hydrate this id at the
	// time of the call — the BFS reached it via the edges table but the
	// nodes-table row lags briefly behind during an active reparse. Lets
	// callers render "unresolved" instead of treating empty name/kind/
	// file_path as a real symbol (solov2-ppk6).
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
	if n.Language != nil {
		d.Language = *n.Language
	}
	return d
}

// nodesToDTO maps a slice of domain nodes, always returning a non-nil slice
// so empty results serialize as [] rather than null/omitted (solov2-elt).
// chunk:* pseudo-nodes are filtered out: they are internal file-fragment
// embeddings used to give un-symbolised code coverage in vector space, and
// surfacing them on a tool that promises "symbols" leaks the abstraction
// (solov2-wbqe).
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

// searchResultToDTO maps an application-layer search.Result onto the wire
// node shape. SymbolPath is the fully-qualified name, so it maps to Name.
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
	// solov2-hl70: compute ScoreNormalized + Tier in one pre-pass over
	// the visible-to-callers result set (chunks excluded — they would
	// otherwise distort the min/max range and the top-of-set tier
	// anchor).
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
		// Filter chunk:* pseudo-nodes (solov2-wbqe) — same reasoning as
		// searchResultsToDTO. Real symbols are the contract; the blast-radius
		// path runs across the graph and would otherwise expose chunk nodes
		// at distance>=1 from any seed in a chunked file.
		if e.Kind == string(domain.KindChunk) {
			continue
		}
		out = append(out, blastEntryToDTO(e))
	}
	return out
}
