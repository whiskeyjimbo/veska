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
type searchHitDTO struct {
	nodeDTO
	Score   float32 `json:"score"`
	Snippet string  `json:"snippet,omitempty"`
}

// blastEntryDTO is a node plus its BFS distance from the nearest seed.
type blastEntryDTO struct {
	nodeDTO
	Distance int    `json:"distance"`
	Snippet  string `json:"snippet,omitempty"`
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
func nodesToDTO(in []*domain.Node) []nodeDTO {
	out := make([]nodeDTO, 0, len(in))
	for _, n := range in {
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
	out := make([]searchHitDTO, 0, len(in))
	for _, r := range in {
		out = append(out, searchResultToDTO(r))
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
	}
}

func blastEntriesToDTO(in []blastradius.Entry) []blastEntryDTO {
	out := make([]blastEntryDTO, 0, len(in))
	for _, e := range in {
		out = append(out, blastEntryToDTO(e))
	}
	return out
}
