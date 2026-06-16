package coverage

import "github.com/whiskeyjimbo/veska/internal/core/domain"

// This file holds SEED-STATE facts: literal operational state a coverage
// harness INSERTS before exercising the registry / task / finding /
// suppression tools. These are NOT parse output and are NOT guarded by the
// drift self-test — there is nothing to re-derive from a parse. They exist so
// the manifest schema spans every fact category the 40 eng_* tools assert.
// RootPath on RepoFact is intentionally empty here: the absolute testdata
// module root is only known at harness time. The harness fills it in (it is
// the same root it passes to NodeKey.ResolveID).

func seedRepos() []RepoFact {
	return []RepoFact{
		{RepoID: AlphaRepoID, ModulePath: AlphaModulePath, Branch: FixtureBranch},
		{RepoID: BetaRepoID, ModulePath: BetaModulePath, Branch: FixtureBranch},
	}
}

func seedAliases() []AliasFact {
	return []AliasFact{
		{Alias: "alpha", RepoID: AlphaRepoID},
		{Alias: "beta", RepoID: BetaRepoID},
	}
}

func seedTasks() []TaskFact {
	return []TaskFact{
		{RepoID: BetaRepoID, TaskID: "task-active", Description: "wire up the badge widget", Active: true},
		{RepoID: BetaRepoID, TaskID: "task-done", Description: "scaffold the beta module", Active: false},
	}
}

// seedFindings are arbitrary findings (distinct from the parse-derived
// TodoFacts) the harness inserts to drive the finding lifecycle tools.
func seedFindings() []FindingFact {
	return []FindingFact{
		{
			RepoID:   AlphaRepoID,
			Rule:     "complexity",
			Severity: "warn",
			Message:  "ComputeVariance has high cyclomatic complexity",
			Anchor:   NodeKey{alphaSeries, domain.KindFunction, "ComputeVariance"},
			State:    "open",
		},
		{
			RepoID:   BetaRepoID,
			Rule:     "style",
			Severity: "info",
			Message:  "badgeHandler ignores the request context",
			Anchor:   NodeKey{betaMain, domain.KindFunction, "badgeHandler"},
			State:    "closed",
		},
	}
}

func seedSuppressions() []SuppressionFact {
	return []SuppressionFact{
		{
			RepoID: AlphaRepoID,
			Rule:   "complexity",
			Anchor: NodeKey{alphaSeries, domain.KindFunction, "ComputeVariance"},
			Reason: "variance loop is intentionally explicit for clarity",
		},
	}
}
