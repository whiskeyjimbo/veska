// SPDX-License-Identifier: AGPL-3.0-only

package graphexport

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ErrMissingDependency is returned by NewService when a required collaborator
// is nil, matching the constructor-returns-typed-error convention used across
// the application layer.
var ErrMissingDependency = errors.New("graphexport: missing dependency")

// LoadGraphFunc loads the full in-memory graph for a repo/branch. Satisfied by
// ports.GraphReader.LoadGraph.
type LoadGraphFunc func(ctx context.Context, repoID, branch string) (*domain.Graph, error)

// RankHotZonesFunc ranks the repo's hot zones. Satisfied by
// wiki.HotZoneService.Rank; repoRoot is needed for its git change-frequency
// pass.
type RankHotZonesFunc func(ctx context.Context, repoID, branch, repoRoot string) (wiki.Report, error)

// SelectEntryPointsFunc selects the repo's entry points. Satisfied by
// wiki.EntryPointsService.Select.
type SelectEntryPointsFunc func(ctx context.Context, repoID, branch string) (wiki.EntryPointsReport, error)

// ListDependenciesFunc lists the external modules the repo calls into.
// Satisfied by dependencies.Service.List - reused verbatim (AC4) rather than
// re-deriving the dependency set here.
type ListDependenciesFunc func(ctx context.Context, repoID, branch string) (dependencies.Result, error)

// LoadSnippetsFunc returns the stored source body for every node, keyed by
// node id. LoadGraph omits node bodies (hot path), so the snapshot hydrates
// raw_content through this separate read. Satisfied by
// sqlite.GraphRepo.NodeSnippets.
type LoadSnippetsFunc func(ctx context.Context, repoID, branch string) (map[string]string, error)

// Service assembles a deterministic graph Snapshot from the existing graph,
// wiki, and dependency primitives. It owns no storage; the four collaborators
// are the only inward dependencies, sized to exactly what the snapshot needs.
type Service struct {
	loadGraph    LoadGraphFunc
	rankHotZones RankHotZonesFunc
	selectEntry  SelectEntryPointsFunc
	listDeps     ListDependenciesFunc
	loadSnippets LoadSnippetsFunc
}

// NewService constructs a Service. All five collaborators are required; a nil
// one yields an error wrapping ErrMissingDependency and a nil *Service.
func NewService(loadGraph LoadGraphFunc, rankHotZones RankHotZonesFunc, selectEntry SelectEntryPointsFunc, listDeps ListDependenciesFunc, loadSnippets LoadSnippetsFunc) (*Service, error) {
	if loadGraph == nil {
		return nil, fmt.Errorf("graphexport.NewService: loadGraph is nil: %w", ErrMissingDependency)
	}
	if rankHotZones == nil {
		return nil, fmt.Errorf("graphexport.NewService: rankHotZones is nil: %w", ErrMissingDependency)
	}
	if selectEntry == nil {
		return nil, fmt.Errorf("graphexport.NewService: selectEntry is nil: %w", ErrMissingDependency)
	}
	if listDeps == nil {
		return nil, fmt.Errorf("graphexport.NewService: listDeps is nil: %w", ErrMissingDependency)
	}
	if loadSnippets == nil {
		return nil, fmt.Errorf("graphexport.NewService: loadSnippets is nil: %w", ErrMissingDependency)
	}
	return &Service{
		loadGraph:    loadGraph,
		rankHotZones: rankHotZones,
		selectEntry:  selectEntry,
		listDeps:     listDeps,
		loadSnippets: loadSnippets,
	}, nil
}

// Export builds the Snapshot for repoID/branch. repoRoot is the repo's
// working-tree path, used both for the hot-zone change-frequency pass and to
// relativize every absolute path written into the snapshot (AC2). The returned
// Snapshot marshals to byte-identical JSON across runs over an unchanged graph
// (AC3): nodes are emitted in graph id order, edges sorted by edge id, and the
// hot-zone / entry-point / dependency lists inherit the deterministic order
// their producing services already guarantee. No wall-clock is embedded.
func (s *Service) Export(ctx context.Context, repoID, branch, repoRoot string) (Snapshot, error) {
	g, err := s.loadGraph(ctx, repoID, branch)
	if err != nil {
		return Snapshot{}, fmt.Errorf("graphexport: load graph: %w", err)
	}
	hot, err := s.rankHotZones(ctx, repoID, branch, repoRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("graphexport: rank hot zones: %w", err)
	}
	eps, err := s.selectEntry(ctx, repoID, branch)
	if err != nil {
		return Snapshot{}, fmt.Errorf("graphexport: select entry points: %w", err)
	}
	deps, err := s.listDeps(ctx, repoID, branch)
	if err != nil {
		return Snapshot{}, fmt.Errorf("graphexport: list dependencies: %w", err)
	}
	snippets, err := s.loadSnippets(ctx, repoID, branch)
	if err != nil {
		return Snapshot{}, fmt.Errorf("graphexport: load snippets: %w", err)
	}

	nodes := projectNodes(g, repoRoot, snippets)
	kept := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		kept[n.ID] = struct{}{}
	}
	snap := Snapshot{
		SchemaVersion: SchemaVersion,
		RepoID:        repoID,
		Branch:        branch,
		Nodes:         nodes,
		Edges:         projectEdges(g, kept),
		HotZones:      projectHotZones(hot, repoRoot),
		EntryPoints:   projectEntryPoints(eps, repoRoot),
		Dependencies:  projectDependencies(deps),
	}
	return snap, nil
}

// projectNodes maps the graph's nodes (already sorted by id) to NodeEntry,
// dropping External nodes, relativizing paths, and hydrating raw_content from
// the separately-loaded snippet map (LoadGraph omits node bodies).
func projectNodes(g *domain.Graph, repoRoot string, snippets map[string]string) []NodeEntry {
	nodes := g.Nodes()
	out := make([]NodeEntry, 0, len(nodes))
	for _, n := range nodes {
		if n.External != nil && *n.External {
			continue
		}
		e := NodeEntry{
			ID:      string(n.ID),
			Name:    n.Name,
			Kind:    string(n.Kind),
			Path:    wiki.RelPath(repoRoot, n.Path),
			Summary: nodeSummary(n),
		}
		if n.Lines != nil {
			e.LineStart = n.Lines.Start
			e.LineEnd = n.Lines.End
		}
		if n.Signature != nil {
			e.Signature = *n.Signature
		}
		if n.Language != nil {
			e.Language = *n.Language
		}
		if n.Exported != nil {
			e.Exported = *n.Exported
		}
		switch {
		case n.RawContent != nil:
			e.RawContent = *n.RawContent
		case snippets != nil:
			e.RawContent = snippets[string(n.ID)]
		}
		out = append(out, e)
	}
	return out
}

// nodeSummary returns the stored ShortSummary, falling back to the
// deterministic HeuristicSummary. This is the same projection rule the MCP
// node DTO applies; it is replicated against the domain method here so the
// application layer holds no dependency on the infrastructure DTO.
func nodeSummary(n *domain.Node) string {
	if n.ShortSummary != nil {
		return *n.ShortSummary
	}
	return n.HeuristicSummary()
}

// projectEdges flattens every node's outgoing edges, drops unresolved
// (proposed) edges and any edge whose endpoint was excluded from the node set
// (e.g. an edge into an External node), and returns the survivors sorted by
// edge id for a stable total order independent of per-node slice ordering.
// Dropping dangling edges keeps the snapshot self-consistent: every edge
// endpoint resolves to a node in nodes[]. External relationships are carried
// by dependencies[] instead.
func projectEdges(g *domain.Graph, kept map[string]struct{}) []EdgeEntry {
	var out []EdgeEntry
	for _, n := range g.Nodes() {
		for _, ed := range g.OutgoingEdges(n.ID) {
			if ed.Confidence == domain.Unresolved {
				continue
			}
			if _, ok := kept[string(ed.Src)]; !ok {
				continue
			}
			if _, ok := kept[string(ed.Tgt)]; !ok {
				continue
			}
			entry := EdgeEntry{
				ID:         ed.ID,
				Src:        string(ed.Src),
				Tgt:        string(ed.Tgt),
				Kind:       string(ed.Kind),
				Confidence: confidenceString(ed.Confidence),
				Resolved:   ed.Resolved,
				Score:      ed.Score,
			}
			if ed.SourceLine != nil {
				entry.SourceLine = *ed.SourceLine
			}
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// confidenceString maps the domain Confidence enum to its snapshot label.
func confidenceString(c domain.Confidence) string {
	switch c {
	case domain.Probable:
		return "probable"
	case domain.Strong:
		return "strong"
	case domain.Definite:
		return "definite"
	default:
		return "unresolved"
	}
}

func projectHotZones(r wiki.Report, repoRoot string) []HotZoneEntry {
	out := make([]HotZoneEntry, 0, len(r.Zones))
	for _, z := range r.Zones {
		out = append(out, HotZoneEntry{
			FilePath:              wiki.RelPath(repoRoot, z.FilePath),
			RecentChangeFrequency: z.RecentChangeFrequency,
			BlastRadius:           z.BlastRadius,
			Score:                 z.Score,
		})
	}
	return out
}

func projectEntryPoints(r wiki.EntryPointsReport, repoRoot string) []EntryPointEntry {
	out := make([]EntryPointEntry, 0, len(r.EntryPoints))
	for _, e := range r.EntryPoints {
		out = append(out, EntryPointEntry{
			SymbolName:      e.SymbolName,
			FilePath:        wiki.RelPath(repoRoot, e.FilePath),
			Kind:            e.Kind,
			InboundCount:    e.InboundCount,
			Exported:        e.Exported,
			HasAdjacentTest: e.HasAdjacentTest,
		})
	}
	return out
}

func projectDependencies(r dependencies.Result) []DependencyEntry {
	out := make([]DependencyEntry, 0, len(r.Dependencies))
	for _, d := range r.Dependencies {
		entry := DependencyEntry{
			Module:      d.Module,
			Version:     d.Version,
			Language:    d.Language,
			UsageCount:  d.UsageCount,
			ImportCount: d.ImportCount,
		}
		for _, c := range d.TopCallSites {
			entry.TopCallSites = append(entry.TopCallSites, DependencyCallSite{
				SrcNodeID:  c.SrcNodeID,
				SymbolPath: c.SymbolPath,
			})
		}
		out = append(out, entry)
	}
	return out
}
