package checks

import (
	"context"
	"fmt"
	"slices"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// UntestedSymbolCheck is a structural check that flags a prod-kind symbol whose
// direct inbound CALLS edges include NO caller in a test-shaped file. With no
// real coverage data, "has a direct caller in a *_test.go-shaped file" is a
// CALLS-edge PROXY for "is covered by tests" (solov2-zvh6.3). It needs no new
// ingest — the signal is latent in CALLS edges already in the graph.
//
// Proxy limits (by design, not bugs — the finding is advisory/low-severity):
//
//   - False positives (flagged untested but actually exercised): interface /
//     dynamic dispatch resolves the CALLS edge to the interface method, not the
//     concrete impl; transitive-only coverage (a test calls A→B→symbol) has no
//     DIRECT test caller; a symbol passed as a function value emits no CALLS
//     edge. These over-report on interface-heavy, table-driven Go.
//   - False negatives (not flagged but effectively untested): a hollow test
//     caller that asserts nothing still emits a CALLS edge.
//
// The bias is toward over-reporting, which is why the finding never blocks on
// its own. The transitive node→test reverse map that removes the transitive
// false positive is tracked as solov2-v6de.1; interface-dispatch suppression
// (borrowing dead-code's InterfaceMethodNames filter) is the natural next
// lever. Neither is in scope here.
//
// Unlike DeadCodeCheck this deliberately does NOT exclude exported symbols: a
// test caller is always a visible CALLS edge regardless of export, so an
// exported prod symbol with no test caller is the HIGHEST-value finding, not a
// false positive. Copying dead-code's exported-symbol exclusion would gut the
// check.
//
// Lifecycle: findings are emitted WITHOUT an anchor content-hash, so they are
// excluded from the content-drift revalidation sweep (which closes via
// revalidate.Decide's default branch — a conservative close that would wrongly
// retire a still-untested symbol the moment its body is edited). A coverage
// proxy changes state when TESTS change, not when the symbol changes, so
// content drift is the wrong axis. Like dead-code (also non-authoritative),
// the consequence is that adding a test later does not auto-close the prior
// finding; a test-caller-predicate revalidation case is the principled fix
// (tracked separately) — until then the finding is stable, not flapping.
type UntestedSymbolCheck struct {
	q ports.CoverageQuerier
	// repoKind, when non-nil, returns the kind ("tracked" / "ephemeral") of a
	// repoID. Ephemeral cache-tier clones (`veska search --repo <url>`)
	// short-circuit to zero findings — mirrors dead-code (solov2-izh6.13).
	// Untested-symbol has strictly worse exposure on an external clone: every
	// prod symbol with no test in the indexed tree would flag.
	repoKind func(ctx context.Context, repoID string) (string, error)
	// ifaceMethods, when non-nil, lists the bare method names declared by every
	// interface in the repo. A concrete method whose bare name matches one is
	// suppressed: it is likely satisfied via interface dispatch, which emits a
	// CALLS edge to the INTERFACE method, not the concrete impl — so a test
	// exercising it through the interface leaves the impl looking untested.
	// This is the same false positive dead-code suppresses (solov2-f1zp); the
	// untested gate is PR-blocking, so silencing it (at the cost of not flagging
	// a genuinely-untested interface method) beats false-FAILing tested code.
	ifaceMethods InterfaceMethodLister
}

// InterfaceMethodLister returns the bare method names declared by every
// interface type in (repoID, branch) — e.g. ["Greet", "String"]. It is the
// narrow capability UntestedSymbolCheck needs to suppress interface-dispatch
// false positives; sqlite.DeadCodeRepo already satisfies it.
type InterfaceMethodLister interface {
	InterfaceMethodNames(ctx context.Context, repoID, branch string) ([]string, error)
}

// UntestedSymbolOption configures an UntestedSymbolCheck.
type UntestedSymbolOption func(*UntestedSymbolCheck)

// WithUntestedInterfaceMethods wires the interface-method lister used to
// suppress interface-dispatch false positives (a concrete method tested only
// through its interface). Strongly recommended for any Go repo.
func WithUntestedInterfaceMethods(l InterfaceMethodLister) UntestedSymbolOption {
	return func(c *UntestedSymbolCheck) { c.ifaceMethods = l }
}

// WithUntestedRepoKindLookup wires a callback returning a repo's Kind
// ("tracked" / "ephemeral"). Run skips reporting on ephemeral repos — the
// siblings the autolink and dead-code short-circuits already skip.
func WithUntestedRepoKindLookup(fn func(ctx context.Context, repoID string) (string, error)) UntestedSymbolOption {
	return func(c *UntestedSymbolCheck) { c.repoKind = fn }
}

// NewUntestedSymbolCheck constructs an UntestedSymbolCheck bound to q. The
// querier is required; passing nil causes Run to return an error on first
// invocation.
func NewUntestedSymbolCheck(q ports.CoverageQuerier, opts ...UntestedSymbolOption) *UntestedSymbolCheck {
	c := &UntestedSymbolCheck{q: q}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name returns the finding-rule / Prometheus attribution name.
func (c *UntestedSymbolCheck) Name() string { return "untested-symbol" }

// Run loads candidate nodes (with their caller file paths) for the input's
// file paths and emits one finding per prod-kind symbol that has no test-file
// caller. An empty Input.FilePaths is a no-op.
func (c *UntestedSymbolCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.q == nil {
		return nil, fmt.Errorf("untested-symbol: nil querier")
	}
	if len(in.FilePaths) == 0 {
		return nil, nil
	}
	// Ephemeral repos (cache-tier clones from `veska search --repo <url>`)
	// skip entirely — reporting "untested" on an external library's symbols
	// trains the reader to ignore the findings surface (mirrors dead-code's
	// izh6.13 short-circuit). Lookup errors fail open (over-report on a tracked
	// repo rather than silently suppress during a registry hiccup).
	if c.repoKind != nil {
		if kind, err := c.repoKind(ctx, in.RepoID); err == nil && kind == "ephemeral" {
			return nil, nil
		}
	}

	cands, err := c.q.CandidateCallersInFiles(ctx, in.RepoID, in.Branch, in.FilePaths)
	if err != nil {
		return nil, fmt.Errorf("untested-symbol: query: %w", err)
	}

	// Interface method names to suppress concrete impls reached via dispatch.
	// An error fails open (over-report) rather than silently widening suppression.
	var ifaceMethods map[string]struct{}
	if c.ifaceMethods != nil {
		if names, ierr := c.ifaceMethods.InterfaceMethodNames(ctx, in.RepoID, in.Branch); ierr == nil && len(names) > 0 {
			ifaceMethods = make(map[string]struct{}, len(names))
			for _, nm := range names {
				ifaceMethods[nm] = struct{}{}
			}
		}
	}

	out := make([]*domain.Finding, 0)
	for _, nc := range cands {
		n := nc.Node
		// Candidate gate: only callable prod kinds, defined in non-test,
		// non-vendored files, excluding entry points (expected-untested noise).
		// NO exported-symbol exclusion — see the type doc.
		if !isDeadCodeCandidate(n) || isTestFile(n.FilePath) || pathfilter.IsVendored(n.FilePath) {
			continue
		}
		if n.Name == "main" || n.Name == "init" {
			continue
		}
		// Suppress a concrete method that satisfies a same-repo interface: a test
		// exercising it via interface dispatch emits no CALLS edge to the impl,
		// so it would otherwise false-FAIL (the persona-test finding).
		if n.Kind == "method" && isInterfaceMethodImpl(n.Name, ifaceMethods) {
			continue
		}
		if hasTestCaller(nc.CallerFiles) {
			continue
		}
		msg := fmt.Sprintf("symbol %q in %s has no test-file caller on branch %s — likely untested (CALLS-edge proxy)",
			n.Name, n.FilePath, in.Branch)
		// No anchor content-hash: see the type doc — content drift is the wrong
		// revalidation axis for a coverage proxy, and setting it would let the
		// drift sweep conservative-close a still-untested symbol on any edit.
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID:   in.RepoID,
			Branch:   in.Branch,
			Severity: domain.SeverityLow,
			Layer:    domain.LayerStructural,
			Rule:     "untested-symbol",
			Message:  msg,
		}, domain.WithNodeAnchor(n.NodeID))
		if err != nil {
			// A malformed node ref should not abort the whole check; skip it.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// hasTestCaller reports whether any caller file path is a test-shaped source.
// The test-file vocabulary lives here (reusing isTestFile), not in the adapter
// SQL, so the language-specific naming rules stay in one trivially-testable
// place — consistent with the dead-code check.
func hasTestCaller(callerFiles []string) bool {
	return slices.ContainsFunc(callerFiles, isTestFile)
}
