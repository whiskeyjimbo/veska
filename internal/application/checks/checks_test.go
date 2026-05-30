package checks_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// recordingStorage captures Save calls in memory.
type recordingStorage struct {
	mu             sync.Mutex
	got            []*domain.Finding
	err            error
	supersedeCalls []supersedeCall
}

func (r *recordingStorage) Save(_ context.Context, f *domain.Finding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, f)
	return nil
}

func (r *recordingStorage) CloseObsolete(_ context.Context, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *recordingStorage) CloseSupersededAutoLinks(_ context.Context, _, _ string, _ []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *recordingStorage) CloseSupersededByRule(_ context.Context, repoID, branch, rule string, keep []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := append([]string(nil), keep...)
	r.supersedeCalls = append(r.supersedeCalls, supersedeCall{repoID: repoID, branch: branch, rule: rule, keep: cp})
	return nil
}

type supersedeCall struct {
	repoID, branch, rule string
	keep                 []string
}

func (r *recordingStorage) snapshot() []*domain.Finding {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Finding, len(r.got))
	copy(out, r.got)
	return out
}

// Compile-time check that recordingStorage implements the port.
var _ ports.FindingStorage = (*recordingStorage)(nil)

// stubCheck is a Check that returns the configured findings (or error/panic).
type stubCheck struct {
	name     string
	findings []*domain.Finding
	err      error
	panicMsg string
	calls    atomic.Int32
}

func (c *stubCheck) Name() string { return c.name }

func (c *stubCheck) Run(_ context.Context, _ checks.Input) ([]*domain.Finding, error) {
	c.calls.Add(1)
	if c.panicMsg != "" {
		panic(c.panicMsg)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.findings, nil
}

func mustFinding(t *testing.T, rule, repoID, branch, filePath string) *domain.Finding {
	t.Helper()
	f, err := domain.NewFinding(domain.FindingSpec{RepoID: repoID, Branch: branch, Severity: domain.SeverityLow, Layer: domain.LayerStructural, Rule: rule, Message: "msg"}, domain.WithFileAnchor(filePath))
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	return f
}

// 1. Runner invokes each registered Check with the input.
func TestRunner_InvokesRegisteredChecks(t *testing.T) {
	c1 := &stubCheck{name: "c1"}
	c2 := &stubCheck{name: "c2"}
	reg := checks.NewRegistry()
	reg.Register(c1)
	reg.Register(c2)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	in := checks.Input{RepoID: "repo1", Branch: "main", GitSHA: "abc", FilePaths: []string{"a.go"}}
	r.Run(context.Background(), in)

	if c1.calls.Load() != 1 {
		t.Errorf("c1 calls = %d, want 1", c1.calls.Load())
	}
	if c2.calls.Load() != 1 {
		t.Errorf("c2 calls = %d, want 1", c2.calls.Load())
	}
}

// 2. Findings returned by a Check are persisted via FindingStorage with source_layer='structural'.
func TestRunner_PersistsFindings(t *testing.T) {
	f := mustFinding(t, "rule1", "repo1", "main", "a.go")
	c := &stubCheck{name: "c1", findings: []*domain.Finding{f}}
	reg := checks.NewRegistry()
	reg.Register(c)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	got := store.snapshot()
	if len(got) != 1 {
		t.Fatalf("findings saved: got %d, want 1", len(got))
	}
	if got[0].SourceLayer != domain.LayerStructural {
		t.Errorf("source_layer = %q, want structural", got[0].SourceLayer)
	}
}

// 3. A Check that errors does NOT abort the runner; other checks still run; findings still saved.
func TestRunner_CheckErrorDoesNotAbort(t *testing.T) {
	bad := &stubCheck{name: "bad", err: errors.New("boom")}
	good := &stubCheck{name: "good", findings: []*domain.Finding{
		mustFinding(t, "ok", "repo1", "main", "a.go"),
	}}
	reg := checks.NewRegistry()
	reg.Register(bad)
	reg.Register(good)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	// Should not panic, should not return error.
	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	if bad.calls.Load() != 1 || good.calls.Load() != 1 {
		t.Errorf("both checks should have run; bad=%d good=%d", bad.calls.Load(), good.calls.Load())
	}
	if got := len(store.snapshot()); got != 1 {
		t.Errorf("findings saved: got %d, want 1", got)
	}
}

// 3b. A Check that panics does NOT abort the runner.
func TestRunner_CheckPanicDoesNotAbort(t *testing.T) {
	bad := &stubCheck{name: "panicky", panicMsg: "kaboom"}
	good := &stubCheck{name: "good"}
	reg := checks.NewRegistry()
	reg.Register(bad)
	reg.Register(good)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	if good.calls.Load() != 1 {
		t.Errorf("good check should have run after panic; calls=%d", good.calls.Load())
	}
}

// 4. Per-check duration is observable via the CheckLatency histogram.
func TestRunner_RecordsPerCheckTiming(t *testing.T) {
	c := &stubCheck{name: "c1"}
	reg := checks.NewRegistry()
	reg.Register(c)

	promReg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(promReg)
	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, metrics)

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	n := testutil.CollectAndCount(metrics.CheckLatency)
	if n == 0 {
		t.Errorf("CheckLatency: expected at least one series after Run, got %d", n)
	}
}

// 5. Empty registry: Run is a no-op.
func TestRunner_EmptyRegistryNoOp(t *testing.T) {
	reg := checks.NewRegistry()
	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	if got := len(store.snapshot()); got != 0 {
		t.Errorf("expected zero findings saved, got %d", got)
	}
}

// authoritativeStub is a stubCheck that also implements
// checks.AuthoritativeChecker, returning the configured rule.
type authoritativeStub struct {
	*stubCheck
	rule string
	on   bool
}

func (a *authoritativeStub) AuthoritativeRule(_ checks.Input) (string, bool) {
	return a.rule, a.on
}

// 6. solov2-jvrc: an authoritative check triggers
// FindingStorage.CloseSupersededByRule with the freshly-returned IDs as
// the keep-set, so prior findings under the same rule that no longer
// apply get auto-closed (e.g. a CVE on a dep that has since been bumped).
func TestRunner_AuthoritativeCheckReconcilesPriorFindings(t *testing.T) {
	f := mustFinding(t, "vulnerable_dependency", "repo1", "main", "go.mod")
	c := &authoritativeStub{
		stubCheck: &stubCheck{name: "vuln-scan", findings: []*domain.Finding{f}},
		rule:      "vulnerable_dependency",
		on:        true,
	}
	reg := checks.NewRegistry()
	reg.Register(c)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.supersedeCalls) != 1 {
		t.Fatalf("CloseSupersededByRule calls = %d, want 1", len(store.supersedeCalls))
	}
	call := store.supersedeCalls[0]
	if call.rule != "vulnerable_dependency" {
		t.Errorf("rule = %q, want vulnerable_dependency", call.rule)
	}
	if call.repoID != "repo1" || call.branch != "main" {
		t.Errorf("scope = (%q, %q), want (repo1, main)", call.repoID, call.branch)
	}
	if len(call.keep) != 1 || call.keep[0] != f.FindingID {
		t.Errorf("keep = %v, want [%s]", call.keep, f.FindingID)
	}
}

// 7. A non-authoritative check must NOT trigger reconciliation, so legacy
// checks that return only a delta keep their additive semantics.
func TestRunner_NonAuthoritativeCheckSkipsReconcile(t *testing.T) {
	f := mustFinding(t, "rule1", "repo1", "main", "a.go")
	c := &stubCheck{name: "c1", findings: []*domain.Finding{f}}
	reg := checks.NewRegistry()
	reg.Register(c)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.supersedeCalls) != 0 {
		t.Errorf("expected no CloseSupersededByRule calls, got %d", len(store.supersedeCalls))
	}
}

// 8. AuthoritativeRule may return ok=false to opt out for a specific
// Input (e.g. ambiguous scope). The Runner must not reconcile in that case.
func TestRunner_AuthoritativeOptOutHonored(t *testing.T) {
	f := mustFinding(t, "vulnerable_dependency", "repo1", "main", "go.mod")
	c := &authoritativeStub{
		stubCheck: &stubCheck{name: "vuln-scan", findings: []*domain.Finding{f}},
		rule:      "vulnerable_dependency",
		on:        false, // opted out
	}
	reg := checks.NewRegistry()
	reg.Register(c)

	store := &recordingStorage{}
	r := checks.NewRunner(reg, store, observability.NewMetrics(prometheus.NewRegistry()))

	r.Run(context.Background(), checks.Input{RepoID: "repo1", Branch: "main"})

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.supersedeCalls) != 0 {
		t.Errorf("opt-out check still triggered reconcile: %+v", store.supersedeCalls)
	}
}
