// SPDX-License-Identifier: AGPL-3.0-only

package revalidate_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// ── fake repo ──────────────────────────────────────────────────────────────

type fakeRepo struct {
	stale    []ports.StaleFinding
	staleErr error
	edgesErr error
	sigErr   error
	// applyErr is returned from ApplyDecisions to simulate a tx-level
	// failure (e.g. Commit fails) so callers can assert metrics-only-on
	// success.
	applyErr error

	// node_id -> hasInbound (dead-code re-run); default false.
	hasInbound map[string]bool
	// node_id -> (prev, current) (contract-drift re-run).
	sigs map[string][2]string
	// node_id -> hasTestCaller (untested-symbol re-run); default false.
	testErr        error
	hasTestCallers map[string]bool

	closedIDs    []string
	closedAt     []int64
	refreshedIDs []string
	refreshedAt  []int64
	refreshedHsh []string

	queryCalls int
	// applyCalls is bumped by ApplyDecisions; analogous to a BeginTx counter
	// since the adapter opens exactly one tx per ApplyDecisions invocation
	// when the batch is non-empty. ApplyDecisions with an empty batch must
	// NOT bump this counter (mirroring the adapter's no-tx fast path).
	applyCalls int
}

func (f *fakeRepo) StaleFindingsForFile(_ context.Context, _, _, _ string) ([]ports.StaleFinding, error) {
	f.queryCalls++
	if f.staleErr != nil {
		return nil, f.staleErr
	}
	return f.stale, nil
}

func (f *fakeRepo) HasInboundCallEdges(_ context.Context, _, _, nodeID string) (bool, error) {
	if f.edgesErr != nil {
		return false, f.edgesErr
	}
	return f.hasInbound[nodeID], nil
}

func (f *fakeRepo) NodeSignaturePair(_ context.Context, _, _, nodeID string) (string, string, error) {
	if f.sigErr != nil {
		return "", "", f.sigErr
	}
	pair := f.sigs[nodeID]
	return pair[0], pair[1], nil
}

func (f *fakeRepo) HasTestCaller(_ context.Context, _, _, nodeID string) (bool, error) {
	if f.testErr != nil {
		return false, f.testErr
	}
	return f.hasTestCallers[nodeID], nil
}

func (f *fakeRepo) ApplyDecisions(_ context.Context, _, _ string, decisions []ports.FindingDecision, at int64) error {
	if len(decisions) == 0 {
		// Mirror adapter contract: empty batch = no tx, no error.
		return nil
	}
	f.applyCalls++
	if f.applyErr != nil {
		return f.applyErr
	}
	for _, d := range decisions {
		switch d.Kind {
		case ports.DecisionRefresh:
			f.refreshedIDs = append(f.refreshedIDs, d.FindingID)
			f.refreshedHsh = append(f.refreshedHsh, d.NewHash)
			f.refreshedAt = append(f.refreshedAt, at)
		case ports.DecisionClose:
			f.closedIDs = append(f.closedIDs, d.FindingID)
			f.closedAt = append(f.closedAt, at)
		}
	}
	return nil
}

// ── unit tests against the fake ────────────────────────────────────────────

func TestHandler_RejectsWrongKind(t *testing.T) {
	t.Parallel()
	h, err := revalidate.NewHandler(&fakeRepo{})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindEmbed, Payload: "x.go"})
	if err == nil {
		t.Fatal("expected error for wrong kind, got nil")
		return
	}
}

func TestHandler_EmptyPayloadIsNoop(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindRevalidate, Payload: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.queryCalls != 0 {
		t.Errorf("expected zero query calls for empty payload, got %d", repo.queryCalls)
	}
}

func TestHandler_NoStaleFindingsIsNoop(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{stale: nil}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.closedIDs) != 0 || len(repo.refreshedIDs) != 0 {
		t.Errorf("expected no closes/refreshes, got closed=%v refreshed=%v", repo.closedIDs, repo.refreshedIDs)
	}
}

func TestHandler_StaleQueryErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-query")
	repo := &fakeRepo{staleErr: sentinel}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_ApplyDecisionsErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-apply")
	repo := &fakeRepo{
		stale:    []ports.StaleFinding{{FindingID: "fA", Rule: "auto-link", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"}},
		applyErr: sentinel,
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_InboundEdgesErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-edges")
	repo := &fakeRepo{
		stale:    []ports.StaleFinding{{FindingID: "fA", Rule: "dead-code", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"}},
		edgesErr: sentinel,
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestHandler_SignaturePairErrorWraps(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom-sig")
	repo := &fakeRepo{
		stale:  []ports.StaleFinding{{FindingID: "fA", Rule: "contract-drift", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"}},
		sigErr: sentinel,
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

// ── per-rule dispatch matrix ───────────────────────────────────────────────

func TestHandler_DeadCode_StillFires_Refreshes(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "dead-code", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
		// hasInbound[n1] absent → false → rule still fires → refresh.
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.refreshedIDs) != 1 || repo.refreshedIDs[0] != "fA" {
		t.Errorf("refreshedIDs = %v, want [fA]", repo.refreshedIDs)
	}
	if repo.refreshedHsh[0] != "h-new" {
		t.Errorf("refreshed hash = %q, want h-new", repo.refreshedHsh[0])
	}
	if len(repo.closedIDs) != 0 {
		t.Errorf("closedIDs = %v, want []", repo.closedIDs)
	}
	if got := testutil.ToFloat64(metrics.RevalidateRefreshed); got != 1 {
		t.Errorf("refreshed counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 0 {
		t.Errorf("closed counter = %v, want 0", got)
	}
}

func TestHandler_DeadCode_NoLongerFires_Closes(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "dead-code", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
		hasInbound: map[string]bool{"n1": true}, // someone calls it now.
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 || repo.closedIDs[0] != "fA" {
		t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
	}
	if len(repo.refreshedIDs) != 0 {
		t.Errorf("refreshedIDs = %v, want []", repo.refreshedIDs)
	}
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 1 {
		t.Errorf("closed counter = %v, want 1", got)
	}
}

func TestHandler_ContractDrift_StillFires_Refreshes(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "contract-drift", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
		sigs: map[string][2]string{"n1": {"sig-a", "sig-b"}}, // prev != cur and prev != "".
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.refreshedIDs) != 1 || repo.refreshedIDs[0] != "fA" {
		t.Errorf("refreshedIDs = %v, want [fA]", repo.refreshedIDs)
	}
	if got := testutil.ToFloat64(metrics.RevalidateRefreshed); got != 1 {
		t.Errorf("refreshed counter = %v, want 1", got)
	}
}

func TestHandler_ContractDrift_Resolved_Closes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prev    string
		current string
	}{
		{"signatures_match", "sig-a", "sig-a"},
		{"no_prev", "", "sig-a"},
		{"both_empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{
				stale: []ports.StaleFinding{
					{FindingID: "fA", Rule: "contract-drift", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
				},
				sigs: map[string][2]string{"n1": {tc.prev, tc.current}},
			}
			h, err := revalidate.NewHandler(repo)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			if err := h.Handle(context.Background(), ports.WorkRow{
				Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
			}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(repo.closedIDs) != 1 || repo.closedIDs[0] != "fA" {
				t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
			}
			if len(repo.refreshedIDs) != 0 {
				t.Errorf("refreshedIDs = %v, want []", repo.refreshedIDs)
			}
		})
	}
}

// The discriminating test: an untested-symbol finding whose
// anchor body drifted but STILL has no test caller must stay open via REFRESH,
// not close. This is the exact failure the old "no anchor hash" doc feared
// the proof Part A (emit anchor hash) is safe because Part B handles the re-run.
func TestHandler_UntestedSymbol_StillUntested_Refreshes(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "untested-symbol", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
		// hasTestCallers[n1] absent → false → still untested → refresh (stays open).
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.refreshedIDs) != 1 || repo.refreshedIDs[0] != "fA" {
		t.Errorf("refreshedIDs = %v, want [fA] (still untested → stays open)", repo.refreshedIDs)
	}
	if repo.refreshedHsh[0] != "h-new" {
		t.Errorf("refreshed hash = %q, want h-new", repo.refreshedHsh[0])
	}
	if len(repo.closedIDs) != 0 {
		t.Errorf("closedIDs = %v, want [] (must NOT conservative-close)", repo.closedIDs)
	}
}

// Now-tested: a test caller appeared, so the rule no longer fires → CLOSE.
func TestHandler_UntestedSymbol_NowTested_Closes(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "untested-symbol", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
		hasTestCallers: map[string]bool{"n1": true}, // a test calls it now.
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 || repo.closedIDs[0] != "fA" {
		t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
	}
	if len(repo.refreshedIDs) != 0 {
		t.Errorf("refreshedIDs = %v, want []", repo.refreshedIDs)
	}
}

// A test-caller predicate error wraps and propagates (queue retries).
func TestHandler_TestCallerErrorWraps(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale:   []ports.StaleFinding{{FindingID: "fA", Rule: "untested-symbol", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"}},
		testErr: errors.New("boom"),
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err == nil {
		t.Fatal("want wrapped error, got nil")
	}
}

func TestHandler_AutoLink_AlwaysCloses(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "auto-link", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 {
		t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
	}
	// Auto-link must not consult inbound edges or signatures.
	if len(repo.refreshedIDs) != 0 {
		t.Errorf("refreshedIDs = %v, want []", repo.refreshedIDs)
	}
}

func TestHandler_UnknownRule_ConservativelyCloses(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "some-future-rule", NodeID: "n1", AnchorHash: "h-old", CurrentHash: "h-new"},
		},
	}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 {
		t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
	}
	if len(repo.refreshedIDs) != 0 {
		t.Errorf("refreshedIDs = %v, want []", repo.refreshedIDs)
	}
}

func TestHandler_MixedBatch_DispatchesPerRule(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "dead-code", NodeID: "n-dead-refresh", CurrentHash: "h-a"},
			{FindingID: "fB", Rule: "dead-code", NodeID: "n-dead-close", CurrentHash: "h-b"},
			{FindingID: "fC", Rule: "contract-drift", NodeID: "n-drift-refresh", CurrentHash: "h-c"},
			{FindingID: "fD", Rule: "contract-drift", NodeID: "n-drift-close", CurrentHash: "h-d"},
			{FindingID: "fE", Rule: "auto-link", NodeID: "n-al", CurrentHash: "h-e"},
			{FindingID: "fF", Rule: "unknown", NodeID: "n-?", CurrentHash: "h-f"},
		},
		hasInbound: map[string]bool{"n-dead-close": true}, // refresh has none.
		sigs: map[string][2]string{
			"n-drift-refresh": {"old", "new"}, // still drifting.
			"n-drift-close":   {"same", "same"},
		},
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	fixed := time.Unix(1700000000, 0)
	h, err := revalidate.NewHandler(repo,
		revalidate.WithClock(func() time.Time { return fixed }),
		revalidate.WithMetrics(metrics),
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantClosed := []string{"fB", "fD", "fE", "fF"}
	wantRefreshed := []string{"fA", "fC"}
	assertStringsEqual(t, "closedIDs", repo.closedIDs, wantClosed)
	assertStringsEqual(t, "refreshedIDs", repo.refreshedIDs, wantRefreshed)

	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != float64(len(wantClosed)) {
		t.Errorf("closed counter = %v, want %d", got, len(wantClosed))
	}
	if got := testutil.ToFloat64(metrics.RevalidateRefreshed); got != float64(len(wantRefreshed)) {
		t.Errorf("refreshed counter = %v, want %d", got, len(wantRefreshed))
	}

	// All timestamps must be the fixed clock value.
	want := fixed.UnixMilli()
	for i, ts := range repo.closedAt {
		if ts != want {
			t.Errorf("closedAt[%d] = %d, want %d", i, ts, want)
		}
	}
	for i, ts := range repo.refreshedAt {
		if ts != want {
			t.Errorf("refreshedAt[%d] = %d, want %d", i, ts, want)
		}
	}
}

func assertStringsEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s len = %d (%v), want %d (%v)", name, len(got), got, len(want), want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}

func TestHandler_NilMetricsIsFunctional(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "auto-link"},
			{FindingID: "fB", Rule: "dead-code"},
		},
	}
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(nil))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.closedIDs) != 1 || repo.closedIDs[0] != "fA" {
		t.Errorf("closedIDs = %v, want [fA]", repo.closedIDs)
	}
	if len(repo.refreshedIDs) != 1 || repo.refreshedIDs[0] != "fB" {
		t.Errorf("refreshedIDs = %v, want [fB]", repo.refreshedIDs)
	}
}

// TestHandler_BatchUsesSingleApplyCall asserts the perf invariant:
// Handle issues exactly one ApplyDecisions call per non-empty file, no
// matter how many stale findings the file contains. This is the
// observable analogue of "one BeginTx per file" in the SQLite adapter.
func TestHandler_BatchUsesSingleApplyCall(t *testing.T) {
	t.Parallel()
	const n = 50
	stale := make([]ports.StaleFinding, 0, n)
	for i := range n {
		stale = append(stale, ports.StaleFinding{
			FindingID:   "f-" + itoa(i),
			Rule:        "auto-link",
			NodeID:      "n-" + itoa(i),
			AnchorHash:  "h-old",
			CurrentHash: "h-new",
		})
	}
	repo := &fakeRepo{stale: stale}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.applyCalls != 1 {
		t.Errorf("applyCalls = %d, want 1 (perf invariant: one tx per file)", repo.applyCalls)
	}
	if len(repo.closedIDs) != n {
		t.Errorf("closedIDs len = %d, want %d", len(repo.closedIDs), n)
	}
}

// TestHandler_EmptyStaleSkipsApply asserts that Handle never invokes
// ApplyDecisions when StaleFindingsForFile returns no rows - no tx is opened
// on a clean file (the dominant case post-sync).
func TestHandler_EmptyStaleSkipsApply(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{stale: nil}
	h, err := revalidate.NewHandler(repo)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.applyCalls != 0 {
		t.Errorf("applyCalls = %d, want 0 (empty stale must skip the tx)", repo.applyCalls)
	}
}

// TestHandler_MetricsOnlyBumpAfterApplyCommits ensures that if
// ApplyDecisions fails (modeling a Commit failure inside the adapter),
// neither the refreshed nor the closed counter advances. The retry path
// re-derives decisions on the next Poller delivery.
func TestHandler_MetricsOnlyBumpAfterApplyCommits(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		stale: []ports.StaleFinding{
			{FindingID: "fA", Rule: "dead-code", NodeID: "n1", CurrentHash: "h-new"},
			{FindingID: "fB", Rule: "auto-link", NodeID: "n2", CurrentHash: "h-new"},
		},
		applyErr: errors.New("commit-failed"),
	}
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(repo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	err = h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "r1", Branch: "main", Payload: "x.go",
	})
	if err == nil {
		t.Fatal("expected error from ApplyDecisions, got nil")
		return
	}
	if got := testutil.ToFloat64(metrics.RevalidateRefreshed); got != 0 {
		t.Errorf("refreshed counter = %v, want 0 (no commit, no metric)", got)
	}
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 0 {
		t.Errorf("closed counter = %v, want 0 (no commit, no metric)", got)
	}
}

// itoa is a tiny helper kept local to avoid pulling in strconv just for
// test-only ID generation.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}

func TestNewHandler_NilRepoErrors(t *testing.T) {
	t.Parallel()
	h, err := revalidate.NewHandler(nil)
	if h != nil {
		t.Errorf("expected nil *Handler, got %v", h)
	}
	if !errors.Is(err, revalidate.ErrMissingDependency) {
		t.Errorf("expected ErrMissingDependency, got %v", err)
	}
}

func TestNewHandler_HappyPath(t *testing.T) {
	t.Parallel()
	h, err := revalidate.NewHandler(&fakeRepo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil *Handler")
	}
}

// ── integration test against real *sql.DB ──────────────────────────────────

// TestHandler_Integration_PerRuleDispatch wires the real SQLite adapter
// behind the handler and exercises the full matrix end-to-end.
func TestHandler_Integration_PerRuleDispatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := openTestDB(t, filepath.Join(dir, "v.db"))

	insertRepo(t, db, "repo1")

	// Nodes:
	//   n-dead-refresh - no inbound edges, content changed.
	//   n-dead-close - has 1 inbound edge, content changed.
	//   n-drift-refresh - prev != current sig, content changed.
	//   n-drift-close - sigs match, content changed.
	//   n-al - content changed (auto-link gets closed regardless).
	//   n-fresh - content matches, finding stays open.
	insertNode(t, db, "n-dead-refresh", "repo1", "main", "pkg/a.go", "h-cur-dr")
	insertNode(t, db, "n-dead-close", "repo1", "main", "pkg/a.go", "h-cur-dc")
	insertNodeWithSig(t, db, "n-drift-refresh", "repo1", "main", "pkg/a.go", "h-cur-drr", "sig-old", "sig-new")
	insertNodeWithSig(t, db, "n-drift-close", "repo1", "main", "pkg/a.go", "h-cur-dcc", "sig-same", "sig-same")
	insertNode(t, db, "n-al", "repo1", "main", "pkg/a.go", "h-cur-al")
	insertNode(t, db, "n-fresh", "repo1", "main", "pkg/a.go", "h-fresh")
	//   n-untested-refresh - no test caller, content changed → stays untested.
	//   n-untested-close - a *_test.go CALLS caller appeared → now tested.
	insertNode(t, db, "n-untested-refresh", "repo1", "main", "pkg/a.go", "h-cur-ur")
	insertNode(t, db, "n-untested-close", "repo1", "main", "pkg/a.go", "h-cur-uc")
	// A "caller" node + edge into n-dead-close.
	insertNode(t, db, "n-caller", "repo1", "main", "pkg/b.go", "h-caller")
	insertEdge(t, db, "edge-1", "repo1", "main", "n-caller", "n-dead-close")
	// A TEST-shaped caller making a CALLS edge into n-untested-close (so the
	// test-caller predicate fires), with the src node living in a *_test.go file.
	insertNode(t, db, "n-test-caller", "repo1", "main", "pkg/a_test.go", "h-tcaller")
	if _, err := db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"edge-test", "main", "repo1", "n-test-caller", "n-untested-close", "CALLS", "definite", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert test-caller edge: %v", err)
	}

	findRepo := sqlite.NewFindingRepo(db)
	revalRepo := sqlite.NewRevalidateRepo(db)

	mustFinding := func(id, rule, nodeID, hash string) *domain.Finding {
		t.Helper()
		f, err := domain.NewFinding(domain.FindingSpec{RepoID: "repo1", Branch: "main", Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: rule, Message: "msg-" + id}, domain.WithNodeAnchor(nodeID),
			domain.WithAnchorContentHash(hash),
		)
		if err != nil {
			t.Fatalf("NewFinding: %v", err)
		}
		return f
	}

	fDeadRefresh := mustFinding("u-dead-r", "dead-code", "n-dead-refresh", "h-anchor-old")
	fDeadClose := mustFinding("u-dead-c", "dead-code", "n-dead-close", "h-anchor-old")
	fDriftRefresh := mustFinding("u-drift-r", "contract-drift", "n-drift-refresh", "h-anchor-old")
	fDriftClose := mustFinding("u-drift-c", "contract-drift", "n-drift-close", "h-anchor-old")
	fAutoLink := mustFinding("u-al", "auto-link", "n-al", "h-anchor-old")
	fFresh := mustFinding("u-fresh", "dead-code", "n-fresh", "h-fresh")
	fUntestedRefresh := mustFinding("u-unt-r", "untested-symbol", "n-untested-refresh", "h-anchor-old")
	fUntestedClose := mustFinding("u-unt-c", "untested-symbol", "n-untested-close", "h-anchor-old")

	for _, fnd := range []*domain.Finding{fDeadRefresh, fDeadClose, fDriftRefresh, fDriftClose, fAutoLink, fFresh, fUntestedRefresh, fUntestedClose} {
		if err := findRepo.Save(context.Background(), fnd); err != nil {
			t.Fatalf("Save %s: %v", fnd.FindingID, err)
		}
	}

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	h, err := revalidate.NewHandler(revalRepo, revalidate.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindRevalidate, RepoID: "repo1", Branch: "main", Payload: "pkg/a.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Verify per-finding state.
	type rowState struct {
		state, reason, anchor string
	}
	get := func(id, branch string) rowState {
		t.Helper()
		var rs rowState
		var reason, anchor sql.NullString
		if err := db.QueryRow(
			`SELECT state, closed_reason, anchor_content_hash FROM findings WHERE finding_id = ? AND branch = ?`,
			id, branch,
		).Scan(&rs.state, &reason, &anchor); err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if reason.Valid {
			rs.reason = reason.String
		}
		if anchor.Valid {
			rs.anchor = anchor.String
		}
		return rs
	}

	// Refreshes: state open, anchor moved to current hash.
	if got := get(fDeadRefresh.FindingID, "main"); got.state != "open" || got.anchor != "h-cur-dr" || got.reason != "" {
		t.Errorf("dead-refresh = %+v, want open/h-cur-dr/no-reason", got)
	}
	if got := get(fDriftRefresh.FindingID, "main"); got.state != "open" || got.anchor != "h-cur-drr" || got.reason != "" {
		t.Errorf("drift-refresh = %+v, want open/h-cur-drr/no-reason", got)
	}
	// Untested, still no test caller → REFRESH (stays open, anchor advances).
	if got := get(fUntestedRefresh.FindingID, "main"); got.state != "open" || got.anchor != "h-cur-ur" || got.reason != "" {
		t.Errorf("untested-refresh = %+v, want open/h-cur-ur/no-reason", got)
	}
	// Closures.
	for _, tc := range []struct {
		id   string
		desc string
	}{
		{fDeadClose.FindingID, "dead-close"},
		{fDriftClose.FindingID, "drift-close"},
		{fAutoLink.FindingID, "autolink-close"},
		{fUntestedClose.FindingID, "untested-close"}, // a _test.go caller now exists
	} {
		got := get(tc.id, "main")
		if got.state != "closed" || got.reason != "revalidated_obsolete" {
			t.Errorf("%s = %+v, want closed/revalidated_obsolete", tc.desc, got)
		}
	}
	// Fresh: untouched (not stale).
	if got := get(fFresh.FindingID, "main"); got.state != "open" || got.anchor != "h-fresh" {
		t.Errorf("fresh = %+v, want open/h-fresh", got)
	}

	if got := testutil.ToFloat64(metrics.RevalidateRefreshed); got != 3 {
		t.Errorf("refreshed counter = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.RevalidateClosed); got != 4 {
		t.Errorf("closed counter = %v, want 4", got)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func openTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertRepo(t *testing.T, db *sql.DB, repoID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/"+repoID, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo %s: %v", repoID, err)
	}
}

func insertNode(t *testing.T, db *sql.DB, nodeID, repoID, branch, filePath, contentHash string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, branch, repoID, "go", "function", nodeID, filePath,
		1, 10, contentHash, time.Now().UnixMilli(), "service:veska", "system",
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func insertNodeWithSig(t *testing.T, db *sql.DB, nodeID, repoID, branch, filePath, contentHash, prevSig, sig string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
        signature, prev_signature
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, branch, repoID, "go", "function", nodeID, filePath,
		1, 10, contentHash, time.Now().UnixMilli(), "service:veska", "system",
		sig, prevSig,
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func insertEdge(t *testing.T, db *sql.DB, edgeID, repoID, branch, src, dst string) {
	t.Helper()
	// Production CALLS edges carry kind 'CALLS' (domain.EdgeCalls); the dead-code
	// liveness predicate (HasInboundCallEdges) matches UPPER(kind)='CALLS', so
	// this caller edge must use that kind to count as liveness.
	_, err := db.Exec(`INSERT INTO edges (
        edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		edgeID, branch, repoID, src, dst, "CALLS", "definite", time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("insert edge %s: %v", edgeID, err)
	}
}
