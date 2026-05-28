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
	"strings"
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
// from. ImportCount is the number of files that name this module in an
// import statement; it can be non-zero even when UsageCount is zero, which
// is the signal that the module is imported but only referenced via
// struct literals / type assertions (solov2-xjm5).
type Dependency struct {
	Module       string     `json:"module"`
	Version      string     `json:"version,omitempty"`
	Language     string     `json:"language"`
	UsageCount   int        `json:"usage_count"`
	ImportCount  int        `json:"import_count,omitempty"`
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

// ImportLister returns one row per (file, import_path) parsed for the
// (repoID, branch). It is the second source the service unions with
// StubAggregator so a module that's imported but only referenced via
// struct literals / type assertions (no resolved CALLS edge) still
// surfaces in `veska deps list` (solov2-xjm5).
//
// An empty result is normal — repos with no parsed imports (or non-Go
// repos until the writer is extended) return ([], nil). A nil
// implementation on the service is treated the same as an empty result.
type ImportLister interface {
	ListImports(ctx context.Context, repoID, branch string) ([]ImportRow, error)
}

// ImportRow is one (file, import_path) entry from file_imports. Language
// matches StubRow.Language so the two sources can be merged without an
// inference step.
type ImportRow struct {
	FilePath   string
	ImportPath string
	Language   string
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
	imports    ImportLister
	versions   ModuleVersionFunc
	repoRoot   RepoRootFunc
	ownModule  OwnModulePathFunc
	topK       int
}

// DefaultTopK is the per-module sample size for top_call_sites. Small
// enough to keep the JSON envelope tight even on highly-fanned-out
// dependencies; an agent that wants more can fall back to
// eng_get_call_chain on a sample node.
const DefaultTopK = 5

// NewService constructs a Service. aggregator is required; versions and
// repoRoot are optional — when nil the dependency's Version field is
// always empty (still useful for ranking + symbol navigation). Pass an
// ImportLister via WithImportLister to union bare-import modules with
// the stub-derived list (solov2-xjm5).
func NewService(aggregator StubAggregator, versions ModuleVersionFunc, repoRoot RepoRootFunc, opts ...ServiceOption) (*Service, error) {
	if aggregator == nil {
		return nil, fmt.Errorf("dependencies.NewService: aggregator is nil: %w", ErrMissingDependency)
	}
	s := &Service{
		aggregator: aggregator,
		versions:   versions,
		repoRoot:   repoRoot,
		topK:       DefaultTopK,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// ServiceOption configures optional Service collaborators. Functional
// options keep NewService callable with the original three-arg signature
// from tests and wire code that don't need the new ports.
type ServiceOption func(*Service)

// WithImportLister supplies the second data source used to union
// bare-import modules into Service.List output (solov2-xjm5).
func WithImportLister(l ImportLister) ServiceOption {
	return func(s *Service) {
		s.imports = l
	}
}

// OwnModulePathFunc returns the importing repo's own module path (e.g.
// "github.com/junior/greetcli") so the service can filter that path and
// its subpackages out of the external-dependency list (solov2-6q1q). An
// empty string disables filtering — useful for non-Go repos or when go.mod
// is absent.
type OwnModulePathFunc func(ctx context.Context, repoRoot string) (string, error)

// WithOwnModulePath supplies the func used to recognise the repo's own
// module path. Without it the "external" dependency list ends up including
// the repo's internal subpackages (anything inside its module tree),
// which is misleading in `veska deps list` output (solov2-6q1q).
func WithOwnModulePath(f OwnModulePathFunc) ServiceOption {
	return func(s *Service) {
		s.ownModule = f
	}
}

// List returns the repo's external dependencies, sorted by usage count
// descending. An empty repo (no stubs) yields a non-nil empty slice so
// callers don't have to special-case JSON marshaling.
func (s *Service) List(ctx context.Context, repoID, branch string) (Result, error) {
	rows, err := s.aggregator.AggregateStubs(ctx, repoID, branch)
	if err != nil {
		return Result{}, fmt.Errorf("dependencies: aggregate stubs: %w", err)
	}

	// Group rows by module_path. importCount is the file-distinct count
	// of import statements naming this module (solov2-xjm5); independent
	// of the call-site count so a module imported but never called still
	// shows up.
	type modAgg struct {
		lang        string
		count       int
		importCount int
		callSites   []CallSite
	}
	byModule := make(map[string]*modAgg)
	// seenSymbol[module] = set of SymbolPaths already represented in
	// callSites — solov2-tpvr: previously a heavily-called symbol consumed
	// every TopK slot ("New, Hello, New, Shout"). Dedupe so the user sees
	// up to TopK *distinct* call-target symbols. UsageCount is still the
	// raw call-site count (every stub row), so popularity ranking is
	// unchanged.
	seenSymbol := make(map[string]map[string]struct{})
	for _, r := range rows {
		m, ok := byModule[r.ModulePath]
		if !ok {
			m = &modAgg{lang: r.Language}
			byModule[r.ModulePath] = m
			seenSymbol[r.ModulePath] = make(map[string]struct{})
		}
		m.count++
		if len(m.callSites) >= s.topK {
			continue
		}
		if _, dup := seenSymbol[r.ModulePath][r.SymbolPath]; dup {
			continue
		}
		seenSymbol[r.ModulePath][r.SymbolPath] = struct{}{}
		m.callSites = append(m.callSites, CallSite{
			SrcNodeID:  r.SrcNodeID,
			SymbolPath: r.SymbolPath,
		})
	}

	// solov2-xjm5: union with parsed imports. A module imported in N files
	// but with zero resolved CALLS edges still surfaces with UsageCount=0
	// and ImportCount=N. Modules already present in the stub-derived map
	// just gain their ImportCount.
	if s.imports != nil {
		impRows, ierr := s.imports.ListImports(ctx, repoID, branch)
		if ierr != nil {
			return Result{}, fmt.Errorf("dependencies: list imports: %w", ierr)
		}
		seenFile := make(map[string]map[string]struct{}, len(byModule))
		for _, r := range impRows {
			if r.ImportPath == "" {
				continue
			}
			files, ok := seenFile[r.ImportPath]
			if !ok {
				files = make(map[string]struct{})
				seenFile[r.ImportPath] = files
			}
			files[r.FilePath] = struct{}{}
			if _, ok := byModule[r.ImportPath]; !ok {
				byModule[r.ImportPath] = &modAgg{lang: r.Language}
			}
		}
		for module, files := range seenFile {
			if m, ok := byModule[module]; ok {
				m.importCount = len(files)
			}
		}
	}

	// Resolve repo root once for version lookups.
	var repoRoot string
	if s.repoRoot != nil && (s.versions != nil || s.ownModule != nil) {
		root, rerr := s.repoRoot(ctx, repoID)
		if rerr != nil {
			return Result{}, fmt.Errorf("dependencies: resolve repo root: %w", rerr)
		}
		repoRoot = root
	}

	// solov2-6q1q: filter the repo's own module path (and its
	// subpackages) out of the external-dependency list. Stub rows can
	// carry intra-module imports when one subpackage imports another
	// (e.g. greetcli/main.go importing greetcli/cmd) — those are not
	// "external" deps and confuse the listing.
	if s.ownModule != nil && repoRoot != "" {
		own, oerr := s.ownModule(ctx, repoRoot)
		if oerr != nil {
			return Result{}, fmt.Errorf("dependencies: resolve own module: %w", oerr)
		}
		if own != "" {
			for module := range byModule {
				if module == own || strings.HasPrefix(module, own+"/") {
					delete(byModule, module)
				}
			}
		}
	}

	deps := make([]Dependency, 0, len(byModule))
	for module, agg := range byModule {
		dep := Dependency{
			Module:       module,
			Language:     agg.lang,
			UsageCount:   agg.count,
			ImportCount:  agg.importCount,
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

// sortDependencies orders by UsageCount desc, then ImportCount desc, then
// Module asc. ImportCount breaks the tie between two stub-zero modules so
// a heavily-imported-but-uncalled module ranks ahead of a once-imported
// one (solov2-xjm5).
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
			if a.UsageCount == b.UsageCount {
				if a.ImportCount > b.ImportCount {
					break
				}
				if a.ImportCount == b.ImportCount && a.Module <= b.Module {
					break
				}
			}
			deps[j-1], deps[j] = deps[j], deps[j-1]
		}
	}
}
