package diffgate_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func deadCodeFinding(t *testing.T, anchorNodeID string) *domain.Finding {
	t.Helper()
	f, err := domain.NewFinding(
		domain.FindingSpec{RepoID: testRepo, Branch: testBranch, Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: "dead-code", Message: "unreferenced symbol"},
		domain.WithNodeAnchor(anchorNodeID),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	return f
}

// indexCandidate builds an Ephemeral for file a.go whose overlay holds the
// given nodes/edges, against the supplied base.
func indexCandidate(t *testing.T, base diffgate.BaseGraph, pr *domain.ParseResult) *diffgate.Ephemeral {
	t.Helper()
	ix, err := diffgate.NewIndexer(&fakeParser{byPath: map[string]*domain.ParseResult{"a.go": pr}})
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	eph, err := ix.Index(context.Background(), testRepo, testBranch, base, staticChangeSource{
		changes: []diffgate.FileChange{{Path: "a.go", Content: []byte("package a")}},
	})
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	return eph
}

// TestVerify_TargetResolved covers AC1: the candidate adds an intra-file caller
// of the dead anchor, so the dead-code rule no longer fires → resolved. The
// caller must be intra-file: a cross-file caller surfaces as an UnresolvedCall
// and (safely) under-resolves to "unresolved" — see ephemeralPredicates.
func TestVerify_TargetResolved(t *testing.T) {
	anchor := "a:Dead"
	callEdge, err := domain.NewEdge(
		domain.EdgeSpec{Src: "a:Caller", Tgt: domain.NodeID(anchor), Kind: domain.EdgeCalls},
		domain.WithConfidence(domain.Definite), // resolved intra-file edge
	)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	// Base: anchor is dead (no inbound edges).
	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{
		Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead"), mustNode(t, "a:Caller", "a.go", "Caller")},
		Edges: []*domain.Edge{callEdge},
	})

	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{Ran: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.ResolutionChecked || !got.TargetResolved {
		t.Fatalf("verdict = %+v, want resolution checked + resolved", got)
	}
}

// TestVerify_TargetUnresolved covers AC2: the candidate leaves the anchor dead
// (no new caller) → still fires → unresolved.
func TestVerify_TargetUnresolved(t *testing.T) {
	anchor := "a:Dead"
	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{
		Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead")},
	})

	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{Ran: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.ResolutionChecked {
		t.Fatalf("resolution should be checked for dead-code; got %+v", got)
	}
	if got.TargetResolved {
		t.Fatalf("verdict = %+v, want unresolved (anchor still dead)", got)
	}
}

// TestVerify_DeadCodeStructuralEdgeNotResolved is the solov2-nmps.9 regression:
// the anchor has an inbound edge in the base, but it is a STRUCTURAL parent edge
// (its package/file CONTAINS it), not a CALLS caller. Dead-code liveness is
// CALLS-only, so the finding must read UNRESOLVED — counting the CONTAINS edge
// reported every dead-code finding resolved with no fix at all, since every
// symbol has a CONTAINS parent.
func TestVerify_DeadCodeStructuralEdgeNotResolved(t *testing.T) {
	anchor := "a:Dead"
	// Base: anchor HAS an inbound edge for all-kind adjacency (its package
	// parent), but ZERO inbound CALLS edges.
	base := &fakeBaseGraph{
		inbound:     map[string][]string{anchor: {"a:Pkg"}},
		callInbound: map[string][]string{anchor: {}},
	}
	// Candidate touches the file but adds no caller of the anchor.
	eph := indexCandidate(t, base, &domain.ParseResult{
		Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead")},
	})

	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{Ran: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.ResolutionChecked {
		t.Fatalf("dead-code is on the allowlist; ResolutionChecked must be true, got %+v", got)
	}
	if got.TargetResolved {
		t.Fatalf("anchor has only a CONTAINS parent edge, no CALLS caller — must read UNRESOLVED, got %+v", got)
	}
}

// TestVerify_NewFindingFails covers AC3: a finding present in the candidate but
// absent in the base is reported as introduced.
func TestVerify_NewFindingFails(t *testing.T) {
	anchor := "a:Dead"
	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead")}})

	introduced := deadCodeFinding(t, "a:NewlyDead")
	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{
		Ran:          true,
		CandidateIDs: []string{introduced.FindingID},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.NewFindingsChecked {
		t.Fatalf("discovery ran; NewFindingsChecked should be true")
	}
	if len(got.NewFindings) != 1 || got.NewFindings[0] != introduced.FindingID {
		t.Fatalf("NewFindings = %v, want [%s]", got.NewFindings, introduced.FindingID)
	}
}

// TestVerify_PreexistingFindingNotNew: a finding present in BOTH base and
// candidate is not "introduced".
func TestVerify_PreexistingFindingNotNew(t *testing.T) {
	anchor := "a:Dead"
	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead")}})
	pre := deadCodeFinding(t, "a:AlreadyDead")

	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{
		Ran:          true,
		BaseIDs:      []string{pre.FindingID},
		CandidateIDs: []string{pre.FindingID},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.NewFindings) != 0 {
		t.Fatalf("NewFindings = %v, want none (finding pre-existed)", got.NewFindings)
	}
}

// TestVerify_DiscoveryNotRunIsDegraded covers the fail-safe: unrun discovery is
// NEVER reported as clean-green; the composer must see NewFindingsChecked=false.
func TestVerify_DiscoveryNotRunIsDegraded(t *testing.T) {
	anchor := "a:Dead"
	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead")}})

	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{Ran: false})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.NewFindingsChecked {
		t.Fatalf("discovery did not run; NewFindingsChecked must be false, got %+v", got)
	}
	if got.NewFindings != nil {
		t.Fatalf("NewFindings should be nil when unchecked, got %v", got.NewFindings)
	}
}

// TestVerify_UnsupportedRuleNotResolved is the safety-critical case: a rule NOT
// on the v1 allowlist (contract-drift) must NOT be routed through Decide and
// read as resolved — that would be a false PASS. It must report
// ResolutionChecked=false.
func TestVerify_UnsupportedRuleNotResolved(t *testing.T) {
	anchor := "a:Sig"
	base := &fakeBaseGraph{}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Sig")}})

	drift, err := domain.NewFinding(
		domain.FindingSpec{RepoID: testRepo, Branch: testBranch, Severity: domain.SeverityMedium, Layer: domain.LayerStructural, Rule: "contract-drift", Message: "signature changed"},
		domain.WithNodeAnchor(anchor),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, drift, diffgate.Discovery{Ran: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ResolutionChecked {
		t.Fatalf("contract-drift is not on the v1 allowlist; ResolutionChecked must be false, got %+v", got)
	}
	if got.TargetResolved {
		t.Fatalf("unsupported rule must never report resolved (false PASS); got %+v", got)
	}
}

// TestVerify_FileAnchoredNotResolved: a file-anchored finding can't be re-run
// via node predicates → ResolutionChecked=false.
func TestVerify_FileAnchoredNotResolved(t *testing.T) {
	base := &fakeBaseGraph{}
	eph := indexCandidate(t, base, &domain.ParseResult{})
	fileFinding, err := domain.NewFinding(
		domain.FindingSpec{RepoID: testRepo, Branch: testBranch, Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "x"},
		domain.WithFileAnchor("a.go"),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	v := diffgate.NewVerifier()
	got, err := v.Verify(context.Background(), eph, fileFinding, diffgate.Discovery{Ran: true})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.ResolutionChecked {
		t.Fatalf("file-anchored finding has no node predicate; ResolutionChecked must be false, got %+v", got)
	}
}
