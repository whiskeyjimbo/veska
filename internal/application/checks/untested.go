// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
// CALLS-edge PROXY for "is covered by tests". It needs no new
// ingest - the signal is latent in CALLS edges already in the graph.
// Proxy limits (by design, not bugs - the finding is advisory/low-severity).
// The complete set was probed empirically in and each case is
// locked by a regression test in coveragegate_probe_test.go:
//
//	SUPPRESSED today: interface / dynamic dispatch resolves the CALLS edge to
//	  the interface method, not the concrete impl - including a base method
//	  reached through embedding when it satisfies an interface. The
//	  InterfaceMethodNames filter below silences these.
//	False positives that REMAIN (no graph signal to suppress on):
//	  · func-value / callback references - a prod fn passed as a value
//	  (fn:= F; F as t.Cleanup(F); a struct field {F}) emits no CALLS edge
//	  · embedded method promotion WITHOUT an interface - w.Do binds to a
//	  non-existent Wrap.Do, not Base.Do;
//	  · transitive-only coverage - a test calls A→B→symbol, no DIRECT test
//	  caller; the principled fix is the transitive reverse map
//	  · reflection / generated harnesses - string-keyed dispatch
//	  (reflect.ValueOf(x).MethodByName("Do").Call(.)) names the symbol only
//	  at runtime, so NO static analysis can ever produce an edge. This is a
//	  PERMANENT proxy limit, not a deferred fix - there is no follow-up bead.
//	False negatives (not flagged but effectively untested): a hollow test
//	  caller that asserts nothing still emits a CALLS edge.
//
// The bias is toward over-reporting. As a FINDING this is advisory/low-severity
// and never blocks on its own; the SEPARATE diff-gate (RunUntested) does return
// ErrGateFailed on a changed-and-untested symbol, which is why suppressing these
// false positives matters there. The fixable false positives each need a NEW
// graph signal (a
// reference edge kind, embedding resolution, or the transitive reverse map)
// not the name-based suppression this check already does - so they are tracked
// as follow-ups; reflection is unanalyzable and stays a permanent limit.
// Unlike DeadCodeCheck this deliberately does NOT exclude exported symbols: a
// test caller is always a visible CALLS edge regardless of export, so an
// exported prod symbol with no test caller is the HIGHEST-value finding, not a
// false positive. Copying dead-code's exported-symbol exclusion would gut the
// check.
// Lifecycle: findings are emitted WITH an anchor content-hash,
// so the post-promotion revalidation sweep selects them when the symbol's body
// drifts - and revalidate.Decide's "untested-symbol" case re-runs the
// test-caller predicate: CLOSE if the symbol now has a test caller, else REFRESH
// in place (still untested, stays open). This makes untested-symbol a structural
// clone of dead-code's lifecycle. The anchor hash was deliberately withheld
// UNTIL that Decide case existed, because without it the sweep's default branch
// would conservative-close a still-untested symbol on any edit. The two are
// atomic: the hash here is only correct because the Decide case exists.
// It remains non-authoritative on the same axis dead-code is: a coverage proxy
// changes state when TESTS change, but the sweep is triggered by the SYMBOL's
// file drifting, so adding a test in another file does not auto-close until the
// symbol is next re-promoted. Stable, not flapping.
type UntestedSymbolCheck struct {
	q ports.CoverageQuerier
	// repoKind, when non-nil, returns the kind ("tracked" / "ephemeral") of a
	// repoID. Ephemeral cache-tier clones (`veska search --repo <url>`)
	// short-circuit to zero findings - mirrors dead-code.
	// Untested-symbol has strictly worse exposure on an external clone: every
	// prod symbol with no test in the indexed tree would flag.
	repoKind func(ctx context.Context, repoID string) (string, error)
	// ifaceMethods, when non-nil, lists the bare method names declared by every
	// interface in the repo. A concrete method whose bare name matches one is
	// suppressed: it is likely satisfied via interface dispatch, which emits a
	// CALLS edge to the INTERFACE method, not the concrete impl - so a test
	// exercising it through the interface leaves the impl looking untested.
	// This is the same false positive dead-code suppresses; the
	// untested gate is PR-blocking, so silencing it (at the cost of not flagging
	// a genuinely-untested interface method) beats false-FAILing tested code.
	ifaceMethods InterfaceMethodLister
}

// InterfaceMethodLister returns the bare method names declared by every
// interface type in (repoID, branch) - e.g. ["Greet", "String"]. It is the
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
// ("tracked" / "ephemeral"). Run skips reporting on ephemeral repos - the
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
	// skip entirely - reporting "untested" on an external library's symbols
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
		// NO exported-symbol exclusion - see the type doc.
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
		msg := fmt.Sprintf("symbol %q in %s has no test-file caller on branch %s - likely untested (CALLS-edge proxy)",
			n.Name, n.FilePath, in.Branch)
		// Anchor on the symbol's content-hash so the revalidation sweep picks the
		// finding up when the body drifts and re-runs the test-caller predicate
		// (revalidate.Decide "untested-symbol" case) - see the type doc. Mirrors
		// dead-code (deadcode.go); empty hash falls back to no-anchor.
		opts := []domain.FindingOption{domain.WithNodeAnchor(n.NodeID)}
		if n.ContentHash != "" {
			opts = append(opts, domain.WithAnchorContentHash(n.ContentHash))
		}
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID:   in.RepoID,
			Branch:   in.Branch,
			Severity: domain.SeverityLow,
			Layer:    domain.LayerStructural,
			Rule:     "untested-symbol",
			Message:  msg,
		}, opts...)
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
// place - consistent with the dead-code check.
func hasTestCaller(callerFiles []string) bool {
	return slices.ContainsFunc(callerFiles, isTestFile)
}
