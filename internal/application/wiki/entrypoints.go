package wiki

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// DefaultMaxBlastRadius is the blast-radius entry-count ceiling a symbol
// must stay under to qualify as an entry point when no WithMaxBlastRadius
// option is set. A low ceiling keeps the surface to genuinely low-risk
// starting points.
const DefaultMaxBlastRadius = 10

// LoadGraphFunc loads the full in-memory code graph for (repoID, branch).
// It mirrors ports.GraphStorage.LoadGraph so the real adapter plugs in
// while tests pass a deterministic fake.
type LoadGraphFunc func(ctx context.Context, repoID, branch string) (*domain.Graph, error)

// InboundEdgesFunc returns, per node ID, the src node IDs of inbound
// edges. It mirrors ports.EdgeReader.InboundEdges so the SQLite adapter
// plugs in while tests pass a fake.
type InboundEdgesFunc func(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)

// OpenFindingsFunc returns the set of node IDs carrying an open finding.
// It mirrors ports.FindingQuerier.OpenFindingNodeIDs.
type OpenFindingsFunc func(ctx context.Context, repoID, branch string) (map[string]bool, error)

// EntryPoint is one selected low-risk symbol a newcomer or agent can
// safely start from: its name, source location and measured blast radius.
type EntryPoint struct {
	SymbolName  string `json:"symbol_name"`
	FilePath    string `json:"file_path"`
	Kind        string `json:"kind"`
	BlastRadius int    `json:"blast_radius"`
}

// EntryPointsReport is the selected entry_points surface. It is the
// structure both the Markdown page and the eng_get_entry_points MCP tool
// are built from, so the two never diverge.
type EntryPointsReport struct {
	RepoID      string       `json:"repo_id"`
	Branch      string       `json:"branch"`
	EntryPoints []EntryPoint `json:"entry_points"`
}

// EntryPointsService selects entry-point symbols. It is stateless; the
// same instance is safe for concurrent callers.
type EntryPointsService struct {
	loadGraph      LoadGraphFunc
	inboundEdges   InboundEdgesFunc
	openFindings   OpenFindingsFunc
	blast          *blastradius.Service
	maxBlastRadius int
}

// EntryPointOption configures an EntryPointsService at construction time.
type EntryPointOption func(*EntryPointsService)

// WithMaxBlastRadius sets the blast-radius entry-count ceiling a symbol
// must stay under to qualify. Non-positive values are ignored so the
// DefaultMaxBlastRadius stays in effect.
func WithMaxBlastRadius(k int) EntryPointOption {
	return func(s *EntryPointsService) {
		if k > 0 {
			s.maxBlastRadius = k
		}
	}
}

// NewEntryPointsService constructs an EntryPointsService. loadGraph,
// inboundEdges, openFindings and blast are all required; a nil dependency
// yields an error wrapping ErrMissingDependency and a nil service.
func NewEntryPointsService(loadGraph LoadGraphFunc, inboundEdges InboundEdgesFunc, openFindings OpenFindingsFunc, blast *blastradius.Service, opts ...EntryPointOption) (*EntryPointsService, error) {
	if loadGraph == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: loadGraph is nil: %w", ErrMissingDependency)
	}
	if inboundEdges == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: inboundEdges is nil: %w", ErrMissingDependency)
	}
	if openFindings == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: openFindings is nil: %w", ErrMissingDependency)
	}
	if blast == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: blast is nil: %w", ErrMissingDependency)
	}
	s := &EntryPointsService{
		loadGraph:      loadGraph,
		inboundEdges:   inboundEdges,
		openFindings:   openFindings,
		blast:          blast,
		maxBlastRadius: DefaultMaxBlastRadius,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Select computes the entry_points report for (repoID, branch). A symbol
// qualifies as an entry point when all three gates hold:
//
//   - it has an adjacent test: an inbound edge from a node whose file path
//     ends in "_test.go";
//   - its blast radius (blastradius.Service entry count) is at or below
//     the configured maximum;
//   - it carries no open finding.
//
// Entry points are ordered by ascending blast radius, then by ascending
// symbol name, so rendering a fixed promoted state twice is byte-identical.
func (s *EntryPointsService) Select(ctx context.Context, repoID, branch string) (EntryPointsReport, error) {
	graph, err := s.loadGraph(ctx, repoID, branch)
	if err != nil {
		return EntryPointsReport{}, fmt.Errorf("wiki: load graph: %w", err)
	}
	if graph == nil {
		return EntryPointsReport{RepoID: repoID, Branch: branch}, nil
	}

	nodes := graph.Nodes() // deterministic ascending-ID order

	flagged, err := s.openFindings(ctx, repoID, branch)
	if err != nil {
		return EntryPointsReport{}, fmt.Errorf("wiki: open findings: %w", err)
	}

	// Only symbol-bearing nodes are candidates — files/packages/fields are
	// not "symbols a newcomer starts from".
	candidates := make([]*domain.Node, 0, len(nodes))
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if !isEntryPointKind(n.Kind) {
			continue
		}
		if flagged[string(n.ID)] {
			continue
		}
		candidates = append(candidates, n)
		ids = append(ids, string(n.ID))
	}

	inbound, err := s.inboundEdges(ctx, repoID, branch, ids)
	if err != nil {
		return EntryPointsReport{}, fmt.Errorf("wiki: inbound edges: %w", err)
	}

	entries := make([]EntryPoint, 0, len(candidates))
	for _, n := range candidates {
		if !hasAdjacentTest(graph, inbound[string(n.ID)]) {
			continue
		}
		resp, err := s.blast.Of(ctx, repoID, branch, []string{string(n.ID)}, blastradius.Options{})
		if err != nil {
			return EntryPointsReport{}, fmt.Errorf("wiki: blast radius for %s: %w", n.ID, err)
		}
		radius := len(resp.Entries)
		if radius > s.maxBlastRadius {
			continue
		}
		entries = append(entries, EntryPoint{
			SymbolName:  n.Name,
			FilePath:    n.Path,
			Kind:        string(n.Kind),
			BlastRadius: radius,
		})
	}

	// Ascending radius; ascending symbol name on ties — fully deterministic.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].BlastRadius != entries[j].BlastRadius {
			return entries[i].BlastRadius < entries[j].BlastRadius
		}
		return entries[i].SymbolName < entries[j].SymbolName
	})
	return EntryPointsReport{RepoID: repoID, Branch: branch, EntryPoints: entries}, nil
}

// isEntryPointKind reports whether a node kind names a symbol a developer
// can meaningfully start from. Containers (file, package, module) and
// fields are excluded.
func isEntryPointKind(k domain.NodeKind) bool {
	switch k {
	case domain.KindFunction, domain.KindMethod, domain.KindType,
		domain.KindStruct, domain.KindInterface, domain.KindClass:
		return true
	default:
		return false
	}
}

// hasAdjacentTest reports whether any inbound src node lives in a Go test
// file (_test.go). Src node IDs are resolved against the graph; an unknown
// ID is skipped.
func hasAdjacentTest(graph *domain.Graph, srcIDs []string) bool {
	for _, srcID := range srcIDs {
		src, ok := graph.Node(domain.NodeID(srcID))
		if !ok {
			continue
		}
		if strings.HasSuffix(src.Path, "_test.go") {
			return true
		}
	}
	return false
}
