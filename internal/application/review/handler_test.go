package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakeGenerator is an in-memory ports.LLMGenerator fixture. It records every
// request it receives and is safe for concurrent use so it can be exercised
// from the poller goroutine.
type fakeGenerator struct {
	mu    sync.Mutex
	reqs  []ports.GenerateRequest
	reply string
	err   error
}

func (f *fakeGenerator) Generate(_ context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return ports.GenerateResponse{}, f.err
	}
	return ports.GenerateResponse{Text: f.reply}, nil
}

func (f *fakeGenerator) calls() []ports.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.GenerateRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// staticRoot returns a RepoRootFunc resolving every repoID to root.
func staticRoot(root string) RepoRootFunc {
	return func(_ context.Context, _ string) (string, error) { return root, nil }
}

// fakeFindingStorage is an in-memory ports.FindingStorage fixture. It records
// every Saved finding and is safe for concurrent use.
type fakeFindingStorage struct {
	mu    sync.Mutex
	saved []*domain.Finding
	err   error
}

func (f *fakeFindingStorage) Save(_ context.Context, fn *domain.Finding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	// Mirror production FindingStorage idempotency on (finding_id, branch).
	for _, ex := range f.saved {
		if ex.FindingID == fn.FindingID && ex.Branch == fn.Branch {
			return nil
		}
	}
	f.saved = append(f.saved, fn)
	return nil
}

func (f *fakeFindingStorage) CloseObsolete(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeFindingStorage) findings() []*domain.Finding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Finding, len(f.saved))
	copy(out, f.saved)
	return out
}

// TestNewHandler_NilDependency proves a nil collaborator wraps
// ErrMissingDependency and yields a nil handler.
func TestNewHandler_NilDependency(t *testing.T) {
	t.Parallel()
	loader, err := NewLoader()
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	cases := map[string]func() (*Handler, error){
		"nil gen": func() (*Handler, error) { return NewHandler(nil, loader, staticRoot("/tmp"), &fakeFindingStorage{}) },
		"nil loader": func() (*Handler, error) {
			return NewHandler(&fakeGenerator{}, nil, staticRoot("/tmp"), &fakeFindingStorage{})
		},
		"nil repoRoot": func() (*Handler, error) { return NewHandler(&fakeGenerator{}, loader, nil, &fakeFindingStorage{}) },
		"nil findings": func() (*Handler, error) { return NewHandler(&fakeGenerator{}, loader, staticRoot("/tmp"), nil) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, err := build()
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("err = %v, want ErrMissingDependency", err)
			}
			if h != nil {
				t.Errorf("handler = %v, want nil", h)
			}
		})
	}
}

// TestHandler_WrongKind proves a misrouted row returns a wrapped error.
func TestHandler_WrongKind(t *testing.T) {
	t.Parallel()
	loader, _ := NewLoader()
	h, err := NewHandler(&fakeGenerator{}, loader, staticRoot(t.TempDir()), &fakeFindingStorage{})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindWiki}); err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

// TestHandler_DispatchesThroughGenerator verifies AC2: the lane renders the
// loaded review prompt, calls Generate with the prompt template version, and
// parses the response.
func TestHandler_DispatchesThroughGenerator(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	loader, err := NewLoader()
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	gen := &fakeGenerator{reply: "NO FINDINGS"}
	h, err := NewHandler(gen, loader, staticRoot(root), &fakeFindingStorage{})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	calls := gen.calls()
	if len(calls) != len(loader.Kinds()) {
		t.Fatalf("Generate called %d times, want %d (one per kind)", len(calls), len(loader.Kinds()))
	}
	for _, c := range calls {
		if c.Prompt == "" {
			t.Error("Generate called with empty rendered prompt")
		}
		if c.PromptTemplateVersion == "" {
			t.Error("Generate called without PromptTemplateVersion")
		}
	}
}

// TestHandler_EmptyPayload proves a row with no file path drains cleanly.
func TestHandler_EmptyPayload(t *testing.T) {
	t.Parallel()
	loader, _ := NewLoader()
	gen := &fakeGenerator{}
	h, _ := NewHandler(gen, loader, staticRoot(t.TempDir()), &fakeFindingStorage{})
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindReview}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gen.calls()) != 0 {
		t.Errorf("Generate called %d times for empty payload, want 0", len(gen.calls()))
	}
}

// TestHandler_GeneratorErrorPropagates proves an LLM failure surfaces as a
// wrapped error so the poller's retry path runs.
func TestHandler_GeneratorErrorPropagates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	sentinel := errors.New("model down")
	h, _ := NewHandler(&fakeGenerator{err: sentinel}, loader, staticRoot(root), &fakeFindingStorage{})
	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped model-down error", err)
	}
}

// TestHandler_FinalAttemptEmitsFailureFinding verifies AC1: a review job that
// fails on its FINAL attempt (row.Attempts >= 3) emits exactly one
// review-pipeline-failure Finding — severity high, source_layer quality,
// node_id anchored on the promotion commit — before returning the job error.
func TestHandler_FinalAttemptEmitsFailureFinding(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	sentinel := errors.New("model down")
	fs := &fakeFindingStorage{}
	h, _ := NewHandler(&fakeGenerator{err: sentinel}, loader, staticRoot(root), fs)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main",
		GitSHA: "sha-deadbeef", Payload: "a.go", Attempts: 3,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped model-down error", err)
	}

	saved := fs.findings()
	if len(saved) != 1 {
		t.Fatalf("Save called %d times, want exactly 1 on final attempt", len(saved))
	}
	f := saved[0]
	if f.Rule != FailureRule {
		t.Errorf("rule = %q, want %q", f.Rule, FailureRule)
	}
	if f.Severity != domain.SeverityHigh {
		t.Errorf("severity = %q, want high", f.Severity)
	}
	if f.SourceLayer != domain.LayerQuality {
		t.Errorf("source_layer = %q, want quality", f.SourceLayer)
	}
	if f.NodeID == nil || *f.NodeID != "sha-deadbeef" {
		t.Errorf("node_id anchor = %v, want sha-deadbeef", f.NodeID)
	}
	if f.RepoID != "repo1" || f.Branch != "main" {
		t.Errorf("repo/branch = %s/%s, want repo1/main", f.RepoID, f.Branch)
	}
	if want := FailureFindingID("repo1", "main", "sha-deadbeef"); f.FindingID != want {
		t.Errorf("finding_id = %q, want %q", f.FindingID, want)
	}
}

// TestHandler_NonFinalAttemptDoesNotEmit verifies AC1's negative: a failing
// attempt that is NOT the final one (row.Attempts < 3) returns the error for
// the poller to re-queue but does NOT emit a finding.
func TestHandler_NonFinalAttemptDoesNotEmit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	sentinel := errors.New("model down")
	fs := &fakeFindingStorage{}
	h, _ := NewHandler(&fakeGenerator{err: sentinel}, loader, staticRoot(root), fs)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main",
		GitSHA: "sha-deadbeef", Payload: "a.go", Attempts: 2,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped model-down error", err)
	}
	if n := len(fs.findings()); n != 0 {
		t.Errorf("Save called %d times on non-final attempt, want 0", n)
	}
}

// reviewBlock is a model response in the package's block format that the
// parser turns into one ReviewFinding of the given severity.
func reviewBlock(severity, title, message string) string {
	return "SEVERITY: " + severity + "\nTITLE: " + title + "\nMESSAGE: " + message
}

// TestHandler_EmitsReviewFindings verifies AC1: a review job whose model
// output parses into findings persists them as domain.Findings carrying
// source_layer='semantic', a review-* rule, the reviewed file anchor, and
// actor_kind='system'.
func TestHandler_EmitsReviewFindings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	gen := &fakeGenerator{reply: reviewBlock("medium", "SQL injection", "unsanitised input reaches the query")}
	fs := &fakeFindingStorage{}
	h, _ := NewHandler(gen, loader, staticRoot(root), fs)

	if err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	saved := fs.findings()
	// One finding per review kind (security + contract_drift both parse the
	// shared block reply).
	if len(saved) != len(loader.Kinds()) {
		t.Fatalf("Save called %d times, want %d (one per kind)", len(saved), len(loader.Kinds()))
	}
	rules := map[string]bool{}
	for _, f := range saved {
		if f.SourceLayer != domain.LayerSemantic {
			t.Errorf("source_layer = %q, want semantic", f.SourceLayer)
		}
		if f.FilePath == nil || *f.FilePath != "a.go" {
			t.Errorf("file anchor = %v, want a.go", f.FilePath)
		}
		if f.NodeID != nil {
			t.Errorf("node_id = %v, want nil for a file-anchored review finding", *f.NodeID)
		}
		if f.ActorKind == nil || *f.ActorKind != domain.ActorKindSystem {
			t.Errorf("actor_kind = %v, want system", f.ActorKind)
		}
		if f.Severity != domain.SeverityMedium {
			t.Errorf("severity = %q, want medium", f.Severity)
		}
		if want := reviewFindingID(f.Rule, "a.go", "SQL injection"); f.FindingID != want {
			t.Errorf("finding_id = %q, want %q", f.FindingID, want)
		}
		rules[f.Rule] = true
	}
	if !rules[RuleSecurity] || !rules[RuleContractDrift] {
		t.Errorf("rules = %v, want both %q and %q", rules, RuleSecurity, RuleContractDrift)
	}
}

// TestHandler_ReviewFindingsAreIdempotent verifies re-reviewing an unchanged
// file reproduces the same finding ids, so FindingStorage.Save collapses the
// repeats.
func TestHandler_ReviewFindingsAreIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	gen := &fakeGenerator{reply: reviewBlock("low", "naming", "exported symbol lacks doc")}
	fs := &fakeFindingStorage{}
	h, _ := NewHandler(gen, loader, staticRoot(root), fs)

	row := ports.WorkRow{Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go"}
	for i := 0; i < 3; i++ {
		if err := h.Handle(context.Background(), row); err != nil {
			t.Fatalf("Handle attempt %d: %v", i, err)
		}
	}
	if n := len(fs.findings()); n != len(loader.Kinds()) {
		t.Errorf("after 3 reviews Save retained %d findings, want %d (idempotent)", n, len(loader.Kinds()))
	}
}

// TestHandler_ReviewFindingSaveErrorPropagates verifies a FindingStorage Save
// failure on the review-finding emit path surfaces as a job error so the
// poller retries — the job is not done if its findings did not persist.
func TestHandler_ReviewFindingSaveErrorPropagates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	dbDown := errors.New("findings db down")
	gen := &fakeGenerator{reply: reviewBlock("high", "auth bypass", "missing access check")}
	fs := &fakeFindingStorage{err: dbDown}
	h, _ := NewHandler(gen, loader, staticRoot(root), fs)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main", Payload: "a.go",
	})
	if !errors.Is(err, dbDown) {
		t.Fatalf("err = %v, want wrapped findings-db-down error", err)
	}
}

// TestHandler_FindingSaveErrorDoesNotMaskJobError verifies a FindingStorage
// failure on the emit path never hides the original job error: the job error
// still surfaces so the poller marks the row failed.
func TestHandler_FindingSaveErrorDoesNotMaskJobError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc A(){}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loader, _ := NewLoader()
	sentinel := errors.New("model down")
	fs := &fakeFindingStorage{err: errors.New("findings db down")}
	h, _ := NewHandler(&fakeGenerator{err: sentinel}, loader, staticRoot(root), fs)

	err := h.Handle(context.Background(), ports.WorkRow{
		Kind: ports.WorkKindReview, RepoID: "repo1", Branch: "main",
		GitSHA: "sha-deadbeef", Payload: "a.go", Attempts: 3,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want original job error to survive a Save failure", err)
	}
}
