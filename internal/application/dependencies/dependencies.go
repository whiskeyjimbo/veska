// Package dependencies lists a repo's external-module imports, ranked by
// call-site frequency derived from cross_repo_edge_stubs.
//
// The stub table is the canonical signal: every external (non-stdlib) call
// site emits a stub keyed by (module_path, symbol_path). Aggregating by
// module_path gives usage counts without re-parsing go.mod, and surfaces
// per-symbol "top call sites" that an agent can land on directly.
//
// Versions come from go.mod require lines via the injected ModuleVersionFunc;
// a missing version is reported empty so the result still ranks (the
// usage_count is what matters).
package dependencies

import (
	"context"
	"errors"
	"fmt"
)

// ErrMissingDependency mirrors the sentinel pattern used by sibling
// services so callers can errors.Is the wrapped constructor error.
var ErrMissingDependency = errors.New("missing dependency")

// CallSite is one call-site reference to an external symbol — the node
// that issued the call and the symbol_path inside the imported module.
type CallSite struct {
	SrcNodeID  string `json:"src_node_id"`
	SymbolPath string `json:"symbol_path"`
}

// Dependency is one external module the repo imports, with the count of
// call sites and (up to TopK) example references for an agent to start
// from.
type Dependency struct {
	Module       string     `json:"module"`
	Version      string     `json:"version,omitempty"`
	Language     string     `json:"language"`
	UsageCount   int        `json:"usage_count"`
	TopCallSites []CallSite `json:"top_call_sites"`
}

// Result is the envelope returned by Service.List. Dependencies are
// sorted by UsageCount desc, then by Module asc for deterministic output.
type Result struct {
	Dependencies []Dependency `json:"dependencies"`
}

// StubAggregator is the narrow port the service consumes — given a (repoID,
// branch), return per-(module, symbol) stub counts plus the source node IDs.
// The single port keeps the application layer decoupled from the SQLite
// adapter and lets tests stub it without spinning up the resolver.
type StubAggregator interface {
	AggregateStubs(ctx context.Context, repoID, branch string) ([]StubRow, error)
}

// StubRow is one (module, symbol, src_node) row from the stubs table.
// The adapter is responsible for the GROUP BY / counting; the service
// composes rows into Dependency entries.
type StubRow struct {
	ModulePath string
	SymbolPath string
	SrcNodeID  string
	Language   string
}

// ModuleVersionFunc looks up a module's version from the importing repo's
// build manifest (go.mod, package.json, …). An empty string + nil error
// signals "not pinned" (no require line / no manifest) — the dependency
// still ranks. An error short-circuits the whole List call.
type ModuleVersionFunc func(ctx context.Context, repoRoot, modulePath string) (string, error)

// RepoRootFunc resolves a repo_id to its working-tree root.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// Service is the application-level facade. It is stateless; the same
// instance is safe for concurrent callers.
type Service struct {
	aggregator StubAggregator
	versions   ModuleVersionFunc
	repoRoot   RepoRootFunc
	topK       int
}

// DefaultTopK is the per-module sample size for top_call_sites. Small
// enough to keep the JSON envelope tight even on highly-fanned-out
// dependencies; an agent that wants more can fall back to
// eng_get_call_chain on a sample node.
const DefaultTopK = 5

// NewService constructs a Service. aggregator is required; versions and
// repoRoot are optional — when nil the dependency's Version field is
// always empty (still useful for ranking + symbol navigation).
func NewService(aggregator StubAggregator, versions ModuleVersionFunc, repoRoot RepoRootFunc) (*Service, error) {
	if aggregator == nil {
		return nil, fmt.Errorf("dependencies.NewService: aggregator is nil: %w", ErrMissingDependency)
	}
	return &Service{
		aggregator: aggregator,
		versions:   versions,
		repoRoot:   repoRoot,
		topK:       DefaultTopK,
	}, nil
}

// List returns the repo's external dependencies, sorted by usage count
// descending. An empty repo (no stubs) yields a non-nil empty slice so
// callers don't have to special-case JSON marshaling.
func (s *Service) List(ctx context.Context, repoID, branch string) (Result, error) {
	rows, err := s.aggregator.AggregateStubs(ctx, repoID, branch)
	if err != nil {
		return Result{}, fmt.Errorf("dependencies: aggregate stubs: %w", err)
	}

	// Group rows by module_path.
	type modAgg struct {
		lang      string
		count     int
		callSites []CallSite
	}
	byModule := make(map[string]*modAgg)
	for _, r := range rows {
		m, ok := byModule[r.ModulePath]
		if !ok {
			m = &modAgg{lang: r.Language}
			byModule[r.ModulePath] = m
		}
		m.count++
		// Cap the per-module call-site sample to keep payloads bounded.
		// The first TopK stub rows win; deterministic because the
		// aggregator returns rows ORDER BY src_node_id (adapter contract).
		if len(m.callSites) < s.topK {
			m.callSites = append(m.callSites, CallSite{
				SrcNodeID:  r.SrcNodeID,
				SymbolPath: r.SymbolPath,
			})
		}
	}

	// Resolve repo root once for version lookups.
	var repoRoot string
	if s.repoRoot != nil && s.versions != nil {
		root, rerr := s.repoRoot(ctx, repoID)
		if rerr != nil {
			return Result{}, fmt.Errorf("dependencies: resolve repo root: %w", rerr)
		}
		repoRoot = root
	}

	deps := make([]Dependency, 0, len(byModule))
	for module, agg := range byModule {
		dep := Dependency{
			Module:       module,
			Language:     agg.lang,
			UsageCount:   agg.count,
			TopCallSites: agg.callSites,
		}
		if s.versions != nil && repoRoot != "" {
			v, verr := s.versions(ctx, repoRoot, module)
			if verr != nil {
				return Result{}, fmt.Errorf("dependencies: version lookup for %s: %w", module, verr)
			}
			dep.Version = v
		}
		deps = append(deps, dep)
	}

	sortDependencies(deps)
	return Result{Dependencies: deps}, nil
}

// sortDependencies orders by UsageCount desc, then Module asc.
func sortDependencies(deps []Dependency) {
	// Stable insertion sort — n is tiny (number of distinct imported
	// modules per repo) and we want deterministic output without
	// pulling in sort.Slice's allocator overhead at this scale.
	for i := 1; i < len(deps); i++ {
		for j := i; j > 0; j-- {
			a, b := deps[j-1], deps[j]
			if a.UsageCount > b.UsageCount {
				break
			}
			if a.UsageCount == b.UsageCount && a.Module <= b.Module {
				break
			}
			deps[j-1], deps[j] = deps[j], deps[j-1]
		}
	}
}
