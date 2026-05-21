package wiki

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

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

// EntryPoint is one selected entry-point symbol: its name, source
// location, and the ranking signals that put it where it landed. The
// JSON shape is the eng_get_entry_points tool surface (solov2-73f).
type EntryPoint struct {
	SymbolName      string `json:"symbol_name"`
	FilePath        string `json:"file_path"`
	Kind            string `json:"kind"`
	InboundCount    int    `json:"inbound_count"`
	Exported        bool   `json:"exported"`
	HasAdjacentTest bool   `json:"has_adjacent_test"`
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
	loadGraph    LoadGraphFunc
	inboundEdges InboundEdgesFunc
	openFindings OpenFindingsFunc
	maxResults   int
}

// EntryPointOption configures an EntryPointsService at construction time.
type EntryPointOption func(*EntryPointsService)

// DefaultMaxResults caps the rendered entry_points list. The page is a
// human-facing surface — beyond ~50 rows the table stops being a useful
// "where do I start" guide.
const DefaultMaxResults = 50

// WithMaxResults caps how many entry points the report returns.
// Non-positive values are ignored so DefaultMaxResults stays in effect.
func WithMaxResults(k int) EntryPointOption {
	return func(s *EntryPointsService) {
		if k > 0 {
			s.maxResults = k
		}
	}
}

// NewEntryPointsService constructs an EntryPointsService. All function
// dependencies are required; a nil dependency yields an error wrapping
// ErrMissingDependency and a nil service.
func NewEntryPointsService(loadGraph LoadGraphFunc, inboundEdges InboundEdgesFunc, openFindings OpenFindingsFunc, opts ...EntryPointOption) (*EntryPointsService, error) {
	if loadGraph == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: loadGraph is nil: %w", ErrMissingDependency)
	}
	if inboundEdges == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: inboundEdges is nil: %w", ErrMissingDependency)
	}
	if openFindings == nil {
		return nil, fmt.Errorf("wiki.NewEntryPointsService: openFindings is nil: %w", ErrMissingDependency)
	}
	s := &EntryPointsService{
		loadGraph:    loadGraph,
		inboundEdges: inboundEdges,
		openFindings: openFindings,
		maxResults:   DefaultMaxResults,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SelectOptions tunes Select per-call. The zero value (IncludeTests=false)
// matches what almost every caller wants: exclude Test*/Benchmark*/
// Example*/Fuzz*-named symbols and *_test.go files from the candidate
// set so the result lists real public-API entry points, not test
// helpers (solov2-m8d).
type SelectOptions struct {
	IncludeTests bool
}

// Select computes the entry_points report for (repoID, branch).
//
// Real entry points have HIGH inbound fan-in: lots of call sites depend
// on them. Exported (capitalised) names in non-test files are likelier
// entry points than internal helpers. The previous gate — "must have
// an adjacent test, must have small blast radius" — selected leaves,
// not entry points (solov2-m8d's close-reason flagged this). Adjacent
// tests are now a tiebreaker bonus, not a hard requirement.
//
// Ranking (solov2-73f):
//  1. inbound_count desc          (the moat: real fan-in)
//  2. exported desc               (capitalised symbols rank above unexported)
//  3. has_adjacent_test desc      (testedness as a tiebreaker)
//  4. symbol_name asc             (final determinism)
//
// Gates remain:
//   - symbol-shaped kind only (function / method / type / struct /
//     interface / class); files/packages/fields excluded;
//   - no open finding on the node;
//   - not a Test*/Benchmark*/Example*/Fuzz* symbol (unless IncludeTests=true).
func (s *EntryPointsService) Select(ctx context.Context, repoID, branch string) (EntryPointsReport, error) {
	return s.SelectWith(ctx, repoID, branch, SelectOptions{})
}

// SelectWith is Select with an explicit SelectOptions; see SelectOptions.
func (s *EntryPointsService) SelectWith(ctx context.Context, repoID, branch string, opts SelectOptions) (EntryPointsReport, error) {
	graph, err := s.loadGraph(ctx, repoID, branch)
	if err != nil {
		return EntryPointsReport{}, fmt.Errorf("wiki: load graph: %w", err)
	}
	if graph == nil {
		return EntryPointsReport{RepoID: repoID, Branch: branch}, nil
	}

	nodes := graph.Nodes()

	flagged, err := s.openFindings(ctx, repoID, branch)
	if err != nil {
		return EntryPointsReport{}, fmt.Errorf("wiki: open findings: %w", err)
	}

	candidates := make([]*domain.Node, 0, len(nodes))
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if !isEntryPointKind(n.Kind) {
			continue
		}
		if flagged[string(n.ID)] {
			continue
		}
		if !opts.IncludeTests && isTestSymbol(n) {
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
		srcIDs := inbound[string(n.ID)]
		entries = append(entries, EntryPoint{
			SymbolName:      n.Name,
			FilePath:        n.Path,
			Kind:            string(n.Kind),
			InboundCount:    len(srcIDs),
			Exported:        isExported(n.Name),
			HasAdjacentTest: hasAdjacentTest(graph, srcIDs),
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.InboundCount != b.InboundCount {
			return a.InboundCount > b.InboundCount
		}
		if a.Exported != b.Exported {
			return a.Exported
		}
		if a.HasAdjacentTest != b.HasAdjacentTest {
			return a.HasAdjacentTest
		}
		return a.SymbolName < b.SymbolName
	})
	if len(entries) > s.maxResults {
		entries = entries[:s.maxResults]
	}
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

// isTestSymbol reports whether a node looks like a Go test/benchmark/
// example/fuzz harness function rather than a real symbol. We check both
// the file path (*_test.go) and the name prefix (Test/Benchmark/Example/
// Fuzz) because either alone leaks: a helper like 'newTestServer' in
// foo_test.go IS in a test file, and a function called 'TestPriority'
// in production code is NOT (solov2-m8d).
func isTestSymbol(n *domain.Node) bool {
	if strings.HasSuffix(n.Path, "_test.go") {
		return true
	}
	switch {
	case strings.HasPrefix(n.Name, "Test"),
		strings.HasPrefix(n.Name, "Benchmark"),
		strings.HasPrefix(n.Name, "Example"),
		strings.HasPrefix(n.Name, "Fuzz"):
		return true
	}
	return false
}

// isExported mirrors Go's capitalised-identifier convention but is
// applied uniformly across languages: a symbol whose first letter is
// uppercase is the language-agnostic signal for "public API surface".
// TS/JS classes (PascalCase) and exported Python functions follow the
// same convention; unexported (lowercase-leading) helpers do not.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return unicode.IsUpper(r)
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
