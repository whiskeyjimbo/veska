package application

import (
	"context"
	"errors"
	"testing"
)

// fakeBranchReader returns a fixed working-tree branch (and optional error).
type fakeBranchReader struct {
	branch string
	err    error
	calls  int
}

func (f *fakeBranchReader) CurrentBranch(rootPath string) (string, error) {
	f.calls++
	return f.branch, f.err
}

// fakeActiveBranchStore records reads and writes of repos.active_branch.
type fakeActiveBranchStore struct {
	active   string
	readErr  error
	setRepo  string
	setVal   string
	setCalls int
}

func (f *fakeActiveBranchStore) ActiveBranch(_ context.Context, _ string) (string, error) {
	return f.active, f.readErr
}

func (f *fakeActiveBranchStore) SetActiveBranch(_ context.Context, repoID, branch string) error {
	f.setCalls++
	f.setRepo = repoID
	f.setVal = branch
	return nil
}

// fakeBumper counts BumpGeneration calls.
type fakeBumper struct {
	gen   uint64
	calls int
}

func (f *fakeBumper) BumpGeneration() uint64 {
	f.calls++
	f.gen++
	return f.gen
}

// fakeClearer records Clear(repoID, branch) calls.
type fakeClearer struct {
	calls  int
	repoID string
	branch string
}

func (f *fakeClearer) Clear(repoID, branch string) {
	f.calls++
	f.repoID = repoID
	f.branch = branch
}

func newFakes(workingBranch, activeBranch string) (*fakeBranchReader, *fakeActiveBranchStore, *fakeBumper, *fakeClearer) {
	return &fakeBranchReader{branch: workingBranch},
		&fakeActiveBranchStore{active: activeBranch},
		&fakeBumper{},
		&fakeClearer{}
}

func TestBranchReconcile_Mismatch_BumpsClearsAndUpdates(t *testing.T) {
	br, store, bump, clear := newFakes("feature/x", "main")
	rc, err := NewBranchReconciler(br, store, bump, clear)
	if err != nil {
		t.Fatalf("NewBranchReconciler: %v", err)
	}

	got, err := rc.Reconcile(context.Background(), "repo1", "/root")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got != "feature/x" {
		t.Errorf("resolved branch = %q, want feature/x", got)
	}

	if bump.calls != 1 {
		t.Errorf("BumpGeneration calls = %d, want 1", bump.calls)
	}
	if clear.calls != 1 || clear.repoID != "repo1" || clear.branch != "main" {
		t.Errorf("Clear = (%d, %q, %q), want (1, repo1, main)", clear.calls, clear.repoID, clear.branch)
	}
	if store.setCalls != 1 || store.setRepo != "repo1" || store.setVal != "feature/x" {
		t.Errorf("SetActiveBranch = (%d, %q, %q), want (1, repo1, feature/x)", store.setCalls, store.setRepo, store.setVal)
	}
}

func TestBranchReconcile_Match_NoOp(t *testing.T) {
	br, store, bump, clear := newFakes("main", "main")
	rc, _ := NewBranchReconciler(br, store, bump, clear)

	got, err := rc.Reconcile(context.Background(), "repo1", "/root")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got != "" {
		t.Errorf("resolved branch on match = %q, want empty", got)
	}

	if bump.calls != 0 {
		t.Errorf("BumpGeneration calls = %d, want 0", bump.calls)
	}
	if clear.calls != 0 {
		t.Errorf("Clear calls = %d, want 0", clear.calls)
	}
	if store.setCalls != 0 {
		t.Errorf("SetActiveBranch calls = %d, want 0", store.setCalls)
	}
}

// A working tree whose branch cannot be determined (detached HEAD, no git)
// must NEVER bump the generation or wipe staging - that would discard valid
// in-flight work on a transient git failure.
func TestBranchReconcile_EmptyBranch_NoOp(t *testing.T) {
	br := &fakeBranchReader{branch: ""}
	store := &fakeActiveBranchStore{active: "main"}
	bump := &fakeBumper{}
	clear := &fakeClearer{}
	rc, _ := NewBranchReconciler(br, store, bump, clear)

	if _, err := rc.Reconcile(context.Background(), "repo1", "/root"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if bump.calls != 0 || clear.calls != 0 || store.setCalls != 0 {
		t.Errorf("empty-branch path mutated state: bump=%d clear=%d set=%d", bump.calls, clear.calls, store.setCalls)
	}
}

func TestBranchReconcile_BranchReadError_NoOp(t *testing.T) {
	br := &fakeBranchReader{err: errors.New("git boom")}
	store := &fakeActiveBranchStore{active: "main"}
	bump := &fakeBumper{}
	clear := &fakeClearer{}
	rc, _ := NewBranchReconciler(br, store, bump, clear)

	if _, err := rc.Reconcile(context.Background(), "repo1", "/root"); err != nil {
		t.Fatalf("Reconcile should swallow git read error, got: %v", err)
	}
	if bump.calls != 0 || clear.calls != 0 || store.setCalls != 0 {
		t.Errorf("read-error path mutated state: bump=%d clear=%d set=%d", bump.calls, clear.calls, store.setCalls)
	}
}

func TestNewBranchReconciler_NilDeps(t *testing.T) {
	br := &fakeBranchReader{}
	store := &fakeActiveBranchStore{}
	bump := &fakeBumper{}
	clear := &fakeClearer{}

	cases := []struct {
		name string
		r    BranchReader
		s    ActiveBranchStore
		b    GenerationBumper
		c    StagingClearer
	}{
		{"nil reader", nil, store, bump, clear},
		{"nil store", br, nil, bump, clear},
		{"nil bumper", br, store, nil, clear},
		{"nil clearer", br, store, bump, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBranchReconciler(tc.r, tc.s, tc.b, tc.c)
			if !errors.Is(err, ErrMissingDependency) {
				t.Fatalf("err = %v, want ErrMissingDependency", err)
			}
		})
	}
}
