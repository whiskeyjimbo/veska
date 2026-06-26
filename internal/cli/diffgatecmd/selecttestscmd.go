// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"errors"
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

// SelectTestsParams are the impact-based test-selection inputs.
// Unlike the diff-gate subcommands this NEVER gates: it emits a runner-consumable
// SELECTION at exit 0. Understandable non-fatal conditions (unknown repo, repo
// not indexed, bad ref) are reported as an empty selection with an `error` field
// in the same JSON envelope, NOT a non-zero exit - only an
// unexpected infra failure (store open, git exec) is returned. It needs the
// indexed base graph to resolve which tests cover the changed prod nodes.
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
// (the safe direction Decision B). Otherwise Tests lists the
// covering test entrypoints and Command applies a `-run` filter.
type packageSelection struct {
	Package string   `json:"package"`
	RunAll  bool     `json:"run_all"`
	Tests   []string `json:"tests,omitempty"`
	Command string   `json:"command"`
}

// selectTestsReport is the JSON envelope. Empty is true when the diff selects no
// tests at all (AC2: say so explicitly, never fall back to "all tests"). Error
// carries an advisory reason (unknown repo, not indexed, bad ref) - the command
// NEVER gates, so these surface as an empty selection + a parseable error field
// at exit 0, not a bare stderr crash.
type selectTestsReport struct {
	Packages []packageSelection `json:"packages"`
	Commands []string           `json:"commands"`
	Empty    bool               `json:"empty"`
	Note     string             `json:"note,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// emitAdvisoryEmpty writes an empty selection carrying an advisory error reason
// and returns nil - the command's "always exits 0, always emits JSON" contract
// for understandable non-fatal conditions. A CI consumer parses
// the same envelope shape whether or not tests were selected.
func emitAdvisoryEmpty(out io.Writer, errMsg string) error {
	return emitSelectTestsReport(out, selectTestsReport{Empty: true, Error: errMsg})
}

// RunSelectTests selects the tests whose covered nodes intersect the candidate
// diff's changed prod nodes, and emits a `go test -run` selection per package.
// Per Decision A the transitive reverse map makes this a DIRECT
// lookup - selected = ⋃ ReverseMap[changed_node] - so there is no blast-radius
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
	p.Branch = resolveBranch(ctx, pools.ReadDB, p.RepoID, p.Branch)
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		// Advisory, not fatal: emit an empty selection + reason at exit 0 so the
		// "always exits 0, emits JSON" contract holds. The reason
		// distinguishes an unknown handle from an unindexed repo (i0tx.2 F2).
		return emitAdvisoryEmpty(p.Out, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
	}

	changedFiles, err := git.ChangedFilesBetween(ctx, p.RepoRoot, p.BaseRef, p.CandidateRef)
	if err != nil {
		// A bad/unknown ref is a user-fixable input error, not a tool failure:
		// surface the cleaned reason as an advisory empty selection (i0tx.2 F3),
		// still at exit 0 with JSON. Genuine git/exec failures stay fatal.
		if errors.Is(err, git.ErrUnknownRevision) {
			return emitAdvisoryEmpty(p.Out, cleanRefError(err, p.BaseRef, p.CandidateRef).Error())
		}
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
		// F4 ( sibling): an empty result is ambiguous - it can mean
		// "your change has no covering tests" OR "this repo has no indexed tests
		// at all (no *_test.go CALLS edges to select from)". Name both so a junior
		// doesn't read it as a false all-clear.
		out.Note = "no covering tests selected for the changed prod nodes (and no test files changed) - note this is also what you see when the repo has no indexed tests at all"
	}
	return out
}

// goTestCommand renders a single package's runner invocation. The `-run` regex
// is anchored `^(.)$` so e.g. TestFoo does not also match TestFoobar - an
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
