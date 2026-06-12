package diffgate_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// gateFixture builds a Gate plus an Ephemeral for a dead-code finding on
// a:Dead. When withCaller is true the candidate adds an intra-file caller
// (resolving the finding); extraNodes are additional overlay nodes (used to
// place a node outside the blast radius). radiusReach is the anchor's blast
// radius for the guard.
func gateFixture(t *testing.T, withCaller bool, extraNodes []string, radiusReach []string) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding) {
	t.Helper()
	anchor := "a:Dead"
	nodes := []*domain.Node{mustNode(t, anchor, "a.go", "Dead")}
	var edges []*domain.Edge
	if withCaller {
		nodes = append(nodes, mustNode(t, "a:Caller", "a.go", "Caller"))
		e, err := domain.NewEdge(
			domain.EdgeSpec{Src: "a:Caller", Tgt: domain.NodeID(anchor), Kind: domain.EdgeCalls},
			domain.WithConfidence(domain.Definite),
		)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		edges = append(edges, e)
	}
	for _, id := range extraNodes {
		nodes = append(nodes, mustNode(t, id, "a.go", id))
	}

	base := &fakeBaseGraph{inbound: map[string][]string{anchor: {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: nodes, Edges: edges})

	guard := newGuard(t, &fakeRadius{reachable: map[string][]string{anchor: radiusReach}})
	gate, err := diffgate.NewGate(diffgate.NewVerifier(), guard)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	return gate, eph, deadCodeFinding(t, anchor)
}

// TestGate_RealRadius_CanonicalDeadCodeFix is the end-to-end proof of ll57.5:
// with a REAL blastradius service, the canonical dead-code fix (add a caller)
// resolves the finding AND passes scope, because the new caller is admitted
// into the allowed set as code wired into the fix — even though the dead
// anchor's base radius is just {anchor}. This is the flagship case the gate
// must NOT over-block.
func TestGate_RealRadius_CanonicalDeadCodeFix(t *testing.T) {
	anchor := "a:Dead"
	base := &fakeBaseGraph{
		metas:   map[string]ports.NodeMeta{anchor: {NodeID: anchor, FilePath: "a.go"}},
		inbound: map[string][]string{anchor: {}},
	}
	callEdge, err := domain.NewEdge(
		domain.EdgeSpec{Src: "a:Caller", Tgt: domain.NodeID(anchor), Kind: domain.EdgeCalls},
		domain.WithConfidence(domain.Definite),
	)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	eph := indexCandidate(t, base, &domain.ParseResult{
		Nodes: []*domain.Node{mustNode(t, anchor, "a.go", "Dead"), mustNode(t, "a:Caller", "a.go", "Caller")},
		Edges: []*domain.Edge{callEdge},
	})
	radius, err := blastradius.NewService(base, base, staging.NewArea())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	guard := newGuard(t, radius)
	gate, err := diffgate.NewGate(diffgate.NewVerifier(), guard)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	v, err := gate.Evaluate(context.Background(), eph, deadCodeFinding(t, anchor), diffgate.Discovery{Ran: true}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Verify.TargetResolved {
		t.Fatalf("expected the fix to resolve the finding; got %+v", v)
	}
	if !v.Pass {
		t.Fatalf("ll57.5: canonical dead-code fix must PASS (new caller wired to anchor); got %+v", v)
	}
}

// TestGate_Pass covers AC1 at the COMPOSER level: resolves within radius, no
// new findings → PASS.
func TestGate_Pass(t *testing.T) {
	gate, eph, target := gateFixture(t, true, nil, []string{"a:Caller"})
	v, err := gate.Evaluate(context.Background(), eph, target, diffgate.Discovery{Ran: true}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass || len(v.Failures) != 0 {
		t.Fatalf("verdict = %+v, want PASS with no failures", v)
	}
	if v.ExitCode() != 0 {
		t.Fatalf("ExitCode = %d, want 0 on PASS", v.ExitCode())
	}
}

// TestGate_FailModes covers AC2: each constituent failure yields FAIL naming
// the failing check, and a non-zero exit (AC3).
func TestGate_FailModes(t *testing.T) {
	tests := []struct {
		name     string
		build    func(t *testing.T) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding)
		disc     diffgate.Discovery
		wantFail string
	}{
		{
			name: "unresolved",
			build: func(t *testing.T) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding) {
				return gateFixture(t, false, nil, nil)
			},
			disc:     diffgate.Discovery{Ran: true},
			wantFail: diffgate.FailUnresolved,
		},
		{
			name: "new_findings",
			build: func(t *testing.T) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding) {
				return gateFixture(t, true, nil, []string{"a:Caller"})
			},
			disc:     diffgate.Discovery{Ran: true, Candidate: []*domain.Finding{deadCodeFinding(t, "a:NewDead")}},
			wantFail: diffgate.FailNewFindings,
		},
		{
			name: "blast_radius_exceeded",
			build: func(t *testing.T) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding) {
				return gateFixture(t, true, []string{"a:Far"}, []string{"a:Caller"})
			},
			disc:     diffgate.Discovery{Ran: true},
			wantFail: diffgate.FailBlastRadiusExceeded,
		},
		{
			name: "discovery_unchecked",
			build: func(t *testing.T) (*diffgate.Gate, *diffgate.Ephemeral, *domain.Finding) {
				return gateFixture(t, true, nil, []string{"a:Caller"})
			},
			disc:     diffgate.Discovery{Ran: false},
			wantFail: diffgate.FailDiscoveryUnchecked,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gate, eph, target := tc.build(t)
			v, err := gate.Evaluate(context.Background(), eph, target, tc.disc, blastradius.Options{})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if v.Pass {
				t.Fatalf("verdict = %+v, want FAIL", v)
			}
			if !slices.Contains(v.Failures, tc.wantFail) {
				t.Fatalf("Failures = %v, want to contain %q", v.Failures, tc.wantFail)
			}
			if v.ExitCode() != 1 {
				t.Fatalf("ExitCode = %d, want 1 on FAIL", v.ExitCode())
			}
		})
	}
}

// TestGate_FileAnchoredDegrades: a file-anchored target can't be resolution- or
// scope-checked → resolution_unchecked, FAIL, scope skipped.
func TestGate_FileAnchoredDegrades(t *testing.T) {
	base := &fakeBaseGraph{}
	eph := indexCandidate(t, base, &domain.ParseResult{})
	fileFinding, err := domain.NewFinding(
		domain.FindingSpec{RepoID: testRepo, Branch: testBranch, Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: "dead-code", Message: "x"},
		domain.WithFileAnchor("a.go"),
	)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	gate, err := diffgate.NewGate(diffgate.NewVerifier(), newGuard(t, &fakeRadius{}))
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	v, err := gate.Evaluate(context.Background(), eph, fileFinding, diffgate.Discovery{Ran: true}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || !slices.Contains(v.Failures, diffgate.FailResolutionUnchecked) {
		t.Fatalf("verdict = %+v, want FAIL with resolution_unchecked", v)
	}
}

// TestGate_AnchorNotResolvedDegrades: when the anchor doesn't resolve in the
// base graph (blastradius.ErrSeedNotFound), the gate degrades to a named
// failure instead of crashing — the fail-safe the smoke test surfaced.
func TestGate_AnchorNotResolvedDegrades(t *testing.T) {
	base := &fakeBaseGraph{inbound: map[string][]string{"a:Dead": {}}}
	eph := indexCandidate(t, base, &domain.ParseResult{Nodes: []*domain.Node{mustNode(t, "a:Dead", "a.go", "Dead")}})
	guard := newGuard(t, &fakeRadius{err: blastradius.ErrSeedNotFound})
	gate, err := diffgate.NewGate(diffgate.NewVerifier(), guard)
	if err != nil {
		t.Fatalf("NewGate: %v", err)
	}
	v, err := gate.Evaluate(context.Background(), eph, deadCodeFinding(t, "a:Dead"), diffgate.Discovery{Ran: true}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Evaluate should degrade, not error: %v", err)
	}
	if v.Pass || !slices.Contains(v.Failures, diffgate.FailAnchorNotResolved) {
		t.Fatalf("verdict = %+v, want FAIL with anchor_not_resolved", v)
	}
}

// TestGate_VerdictJSON covers AC3: the verdict round-trips as JSON with the
// stable field names CI/agents consume.
func TestGate_VerdictJSON(t *testing.T) {
	gate, eph, target := gateFixture(t, false, nil, nil)
	v, err := gate.Evaluate(context.Background(), eph, target, diffgate.Discovery{Ran: true}, blastradius.Options{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got struct {
		Pass     bool     `json:"pass"`
		Failures []string `json:"failures"`
		Verify   struct {
			TargetResolved bool `json:"target_resolved"`
		} `json:"verify"`
		Scope struct {
			Contained bool `json:"contained"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Pass {
		t.Fatalf("expected pass=false in JSON, got %s", raw)
	}
	if !slices.Contains(got.Failures, diffgate.FailUnresolved) {
		t.Fatalf("JSON failures = %v, want unresolved", got.Failures)
	}
}

func TestNewGate_NilDeps(t *testing.T) {
	if _, err := diffgate.NewGate(nil, newGuard(t, &fakeRadius{})); err == nil {
		t.Fatalf("nil verifier should error")
	}
	if _, err := diffgate.NewGate(diffgate.NewVerifier(), nil); err == nil {
		t.Fatalf("nil guard should error")
	}
}
