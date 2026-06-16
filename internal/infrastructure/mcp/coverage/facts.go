package coverage

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// This file holds the FROZEN, parse-derived facts authored from real pipeline
// output (see dump_test.go's TestDumpFixtureFacts). Chunk nodes are
// deliberately EXCLUDED — their names are line-ranges (chunk:1-5) that couple
// the manifest to exact line numbers and no tool keys on chunk identity.
// Relative paths are in slash form; the parser emits one package node PER FILE
// (so modalpha/metric has two "metric" package nodes, one per.go file).

const (
	alphaSeries    = "metric/series.go"
	alphaDeviation = "metric/deviation.go"
	betaMain       = "main.go"
	betaRender     = "widget/render.go"
)

func frozenNodes() []NodeKey {
	return []NodeKey{
		// modalpha/metric/series.go
		{Path: alphaSeries, Kind: domain.KindPackage, Name: "metric"},
		{Path: alphaSeries, Kind: domain.KindStruct, Name: "Series"},
		{Path: alphaSeries, Kind: domain.KindInterface, Name: "Accumulator"},
		{Path: alphaSeries, Kind: domain.KindMethod, Name: "Accumulator.Add"},
		{Path: alphaSeries, Kind: domain.KindMethod, Name: "Accumulator.Mean"},
		{Path: alphaSeries, Kind: domain.KindFunction, Name: "ComputeVariance"},
		{Path: alphaSeries, Kind: domain.KindFunction, Name: "computeMean"},

		// modalpha/metric/deviation.go
		{Path: alphaDeviation, Kind: domain.KindPackage, Name: "metric"},
		{Path: alphaDeviation, Kind: domain.KindFunction, Name: "averageSamples"},
		{Path: alphaDeviation, Kind: domain.KindFunction, Name: "StandardDeviation"},
		{Path: alphaDeviation, Kind: domain.KindFunction, Name: "sqrtApprox"},

		// modbeta/main.go
		{Path: betaMain, Kind: domain.KindPackage, Name: "main"},
		{Path: betaMain, Kind: domain.KindFunction, Name: "main"},
		{Path: betaMain, Kind: domain.KindFunction, Name: "badgeHandler"},

		// modbeta/widget/render.go
		{Path: betaRender, Kind: domain.KindPackage, Name: "widget"},
		{Path: betaRender, Kind: domain.KindStruct, Name: "Palette"},
		{Path: betaRender, Kind: domain.KindStruct, Name: "Badge"},
		{Path: betaRender, Kind: domain.KindInterface, Name: "Renderer"},
		{Path: betaRender, Kind: domain.KindMethod, Name: "Renderer.Render"},
		{Path: betaRender, Kind: domain.KindMethod, Name: "Badge.RenderBadge"},
		{Path: betaRender, Kind: domain.KindFunction, Name: "formatJitter"},
	}
}

func frozenEdges() []EdgeFact {
	return append(frozenAlphaEdges(), frozenBetaEdges()...)
}

func frozenAlphaEdges() []EdgeFact {
	fn := domain.KindFunction
	mt := domain.KindMethod
	pk := domain.KindPackage
	calls := domain.EdgeCalls
	contains := domain.EdgeContains

	return []EdgeFact{
		// CALLS (intra-module, resolved to concrete edges).
		{RepoID: AlphaRepoID, Kind: calls,
			Src: NodeKey{alphaSeries, fn, "ComputeVariance"},
			Dst: NodeKey{alphaSeries, fn, "computeMean"}},
		{RepoID: AlphaRepoID, Kind: calls,
			Src: NodeKey{alphaDeviation, fn, "StandardDeviation"},
			Dst: NodeKey{alphaSeries, fn, "ComputeVariance"}},
		{RepoID: AlphaRepoID, Kind: calls,
			Src: NodeKey{alphaDeviation, fn, "StandardDeviation"},
			Dst: NodeKey{alphaDeviation, fn, "sqrtApprox"}},

		// CONTAINS (package -> members).
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, domain.KindInterface, "Accumulator"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, mt, "Accumulator.Add"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, mt, "Accumulator.Mean"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, fn, "ComputeVariance"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, domain.KindStruct, "Series"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaSeries, pk, "metric"}, Dst: NodeKey{alphaSeries, fn, "computeMean"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaDeviation, pk, "metric"}, Dst: NodeKey{alphaDeviation, fn, "StandardDeviation"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaDeviation, pk, "metric"}, Dst: NodeKey{alphaDeviation, fn, "averageSamples"}},
		{RepoID: AlphaRepoID, Kind: contains,
			Src: NodeKey{alphaDeviation, pk, "metric"}, Dst: NodeKey{alphaDeviation, fn, "sqrtApprox"}},
	}
}

func frozenBetaEdges() []EdgeFact {
	fn := domain.KindFunction
	mt := domain.KindMethod
	pk := domain.KindPackage
	calls := domain.EdgeCalls
	contains := domain.EdgeContains

	return []EdgeFact{
		// CALLS (intra-module).
		{RepoID: BetaRepoID, Kind: calls,
			Src: NodeKey{betaRender, mt, "Badge.RenderBadge"},
			Dst: NodeKey{betaRender, fn, "formatJitter"}},
		{RepoID: BetaRepoID, Kind: calls,
			Src: NodeKey{betaMain, fn, "badgeHandler"},
			Dst: NodeKey{betaRender, mt, "Badge.RenderBadge"}},
		{RepoID: BetaRepoID, Kind: calls,
			Src: NodeKey{betaMain, fn, "main"},
			Dst: NodeKey{betaMain, fn, "badgeHandler"}},

		// CONTAINS.
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaMain, pk, "main"}, Dst: NodeKey{betaMain, fn, "badgeHandler"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaMain, pk, "main"}, Dst: NodeKey{betaMain, fn, "main"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, domain.KindStruct, "Badge"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, mt, "Badge.RenderBadge"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, domain.KindStruct, "Palette"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, domain.KindInterface, "Renderer"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, mt, "Renderer.Render"}},
		{RepoID: BetaRepoID, Kind: contains,
			Src: NodeKey{betaRender, pk, "widget"}, Dst: NodeKey{betaRender, fn, "formatJitter"}},
	}
}

// frozenCrossRepoEdges: modbeta's Badge.RenderBadge calls modalpha's
// metric.ComputeVariance, which the promoter records as a cross_repo_edge_stub
// because example.com/modalpha/metric is external to modbeta's module.
func frozenCrossRepoEdges() []CrossRepoEdgeFact {
	return []CrossRepoEdgeFact{
		{
			RepoID:     BetaRepoID,
			Kind:       domain.EdgeCalls,
			Src:        NodeKey{betaRender, domain.KindMethod, "Badge.RenderBadge"},
			ModulePath: "example.com/modalpha/metric",
			Symbol:     "ComputeVariance",
		},
	}
}

// frozenDependencies: file_imports rows the promoter persisted. modalpha
// imports nothing external, so only modbeta contributes the one genuine
// cross-module dep. modbeta's own-module import (example.com/modbeta/widget)
// is intentionally absent: made syncFileImports subtract the
// repo's own module_path, so intra-module imports no longer land in
// file_imports / eng_list_dependencies.
func frozenDependencies() []DependencyFact {
	return []DependencyFact{
		{RepoID: BetaRepoID, FromRelPath: betaRender, ImportPath: "example.com/modalpha/metric"},
	}
}

// frozenEntryPoints: only func main qualifies cleanly as a program entry point.
// badgeHandler is parsed as a plain KindFunction (not KindRoute), so whether
// the entry-point selector surfaces it depends on inbound-fan-in heuristics;
// we freeze only the unambiguous main here.
// Note: Alpha's exported ComputeVariance is deliberately NOT a
// frozen entry point. It has inbound>=1 and would otherwise rank, but the
// fixture seeds an OPEN complexity finding on it (seedFindings), and the
// selector's open-finding gate excludes such nodes by design. The tqda
// coverage test asserts that exclusion negatively rather than freezing it here.
func frozenEntryPoints() []EntryPointFact {
	return []EntryPointFact{
		{RepoID: BetaRepoID, Node: NodeKey{betaMain, domain.KindFunction, "main"}},
	}
}

// frozenTodos: one TODO marker per file, surfaced as a rule='todo' finding.
func frozenTodos() []TodoFact {
	return []TodoFact{
		{RepoID: AlphaRepoID, RelPath: alphaSeries, Line: 20, Marker: "TODO",
			Text: "switch to Welford's online algorithm for numerical stability."},
		{RepoID: BetaRepoID, RelPath: betaMain, Line: 14, Marker: "TODO",
			Text: "make the listen address configurable via flag."},
	}
}

// frozenClones: computeMean and averageSamples are the near-duplicate pair.
func frozenClones() []CloneFact {
	return []CloneFact{
		{
			RepoID: AlphaRepoID,
			A:      NodeKey{alphaSeries, domain.KindFunction, "computeMean"},
			B:      NodeKey{alphaDeviation, domain.KindFunction, "averageSamples"},
		},
	}
}
