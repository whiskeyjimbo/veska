package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/coverage"
	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// SelectTestsParams are the impact-based test-selection inputs (solov2-v6de.2).
// Unlike the diff-gate subcommands this NEVER gates: it emits a runner-consumable
// SELECTION and always exits 0 (a real error — bad refs, repo not indexed, git/db
// failure — is still returned). It needs the indexed base graph to resolve which
// tests cover the changed prod nodes.
type SelectTestsParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// packageSelection is one package's slice of the selection. RunAll is set when
// a *_test.go file in the package changed: a newly-added/edited test is not yet
// in the index, so the whole package is run rather than risk under-selecting it
// (the safe direction — solov2-v6de Decision B). Otherwise Tests lists the
// covering test entrypoints and Command applies a `-run` filter.
type packageSelection struct {
	Package string   `json:"package"`
	RunAll  bool     `json:"run_all"`
	Tests   []string `json:"tests,omitempty"`
	Command string   `json:"command"`
}

// selectTestsReport is the JSON envelope. Empty is true when the diff selects no
// tests at all (AC2: say so explicitly, never fall back to "all tests").
type selectTestsReport struct {
	Packages []packageSelection `json:"packages"`
	Commands []string           `json:"commands"`
	Empty    bool               `json:"empty"`
	Note     string             `json:"note,omitempty"`
}

// RunSelectTests selects the tests whose covered nodes intersect the candidate
// diff's changed prod nodes, and emits a `go test -run` selection per package.
//
// Per solov2-v6de Decision A the transitive reverse map makes this a DIRECT
// lookup — selected = ⋃ ReverseMap[changed_node] — so there is no blast-radius
// BFS and no ephemeral clone (the diff-gate's clone exists to restore
// cascade-deleted edges after a re-promote we don't do here). Changed prod files
// seed the reverse map against the LIVE index; changed test files force their
// package to run-all to cover tests not yet indexed.
func RunSelectTests(ctx context.Context, p SelectTestsParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate select-tests: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := config.DefaultVectorDir() + "/veska.db"
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate select-tests: open store: %w", err)
	}
	defer pools.Close()

	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		return fmt.Errorf("diff-gate select-tests: repo %q not indexed on branch %q — index it first (e.g. `veska reindex`)", p.RepoID, p.Branch)
	}

	changedFiles, err := git.ChangedFilesBetween(ctx, p.RepoRoot, p.BaseRef, p.CandidateRef)
	if err != nil {
		return fmt.Errorf("diff-gate select-tests: list changed files: %w", err)
	}

	// Split the diff: prod files seed the reverse map; changed test files force
	// their package (their tests may be net-new and absent from the index).
	var prodChanged []string
	forcedPkgs := make(map[string]struct{})
	for _, f := range changedFiles {
		if pathfilter.IsTestFile(f) {
			forcedPkgs[path.Dir(f)] = struct{}{}
			continue
		}
		prodChanged = append(prodChanged, f)
	}

	// Changed prod files → changed node IDs (file precision: every node in a
	// touched file, the safe over-selecting direction).
	nodeLookup := sqlite.NewNodeLookupRepo(pools.ReadDB)
	seen := make(map[string]struct{})
	var changedNodeIDs []string
	for _, f := range prodChanged {
		ids, lerr := nodeLookup.NodesInFile(ctx, p.RepoID, p.Branch, f)
		if lerr != nil {
			return fmt.Errorf("diff-gate select-tests: nodes in %s: %w", f, lerr)
		}
		for _, id := range ids {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				changedNodeIDs = append(changedNodeIDs, id)
			}
		}
	}

	revMap, err := coverage.NewReverseMap(sqlite.NewCoverageRepo(pools.ReadDB))
	if err != nil {
		return fmt.Errorf("diff-gate select-tests: %w", err)
	}
	tests, err := revMap.TestsCoveringAny(ctx, p.RepoID, p.Branch, changedNodeIDs)
	if err != nil {
		return fmt.Errorf("diff-gate select-tests: %w", err)
	}

	report := buildSelectionReport(tests, forcedPkgs)
	return emitSelectTestsReport(p.Out, report)
}

// buildSelectionReport groups covering tests by package, folds in the forced
// (changed-test-file) packages, and renders a deterministic per-package
// `go test` command. A forced package runs all its tests; the rest run a
// `^(TestA|TestB)$`-anchored subset.
func buildSelectionReport(tests []coverage.TestRef, forcedPkgs map[string]struct{}) selectTestsReport {
	testsByPkg := make(map[string]map[string]struct{})
	for _, t := range tests {
		pkg := path.Dir(t.FilePath)
		if testsByPkg[pkg] == nil {
			testsByPkg[pkg] = make(map[string]struct{})
		}
		testsByPkg[pkg][t.Name] = struct{}{}
	}

	pkgSet := make(map[string]struct{}, len(testsByPkg)+len(forcedPkgs))
	for pkg := range testsByPkg {
		pkgSet[pkg] = struct{}{}
	}
	for pkg := range forcedPkgs {
		pkgSet[pkg] = struct{}{}
	}

	pkgs := make([]string, 0, len(pkgSet))
	for pkg := range pkgSet {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)

	out := selectTestsReport{Empty: len(pkgs) == 0}
	for _, pkg := range pkgs {
		_, forced := forcedPkgs[pkg]
		sel := packageSelection{Package: pkg, RunAll: forced}
		if !forced {
			names := make([]string, 0, len(testsByPkg[pkg]))
			for n := range testsByPkg[pkg] {
				names = append(names, n)
			}
			sort.Strings(names)
			sel.Tests = names
		}
		sel.Command = goTestCommand(pkg, sel.RunAll, sel.Tests)
		out.Packages = append(out.Packages, sel)
		out.Commands = append(out.Commands, sel.Command)
	}
	if out.Empty {
		out.Note = "no covering tests selected for the changed prod nodes (and no test files changed)"
	}
	return out
}

// goTestCommand renders a single package's runner invocation. The `-run` regex
// is anchored `^(...)$` so e.g. TestFoo does not also match TestFoobar — an
// over-select that would masquerade as precision.
func goTestCommand(pkg string, runAll bool, tests []string) string {
	target := "./" + pkg
	if runAll || len(tests) == 0 {
		return "go test " + target
	}
	return fmt.Sprintf("go test -run '^(%s)$' %s", strings.Join(tests, "|"), target)
}

func emitSelectTestsReport(out io.Writer, rep selectTestsReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate select-tests: encode selection: %w", err)
	}
	return nil
}
