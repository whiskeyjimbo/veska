package application

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// fakeCheckRunner records each invocation and the input it received.
type fakeCheckRunner struct {
	calls    atomic.Int32
	lastRepo string
	lastSHA  string
	lastN    int
}

func (f *fakeCheckRunner) Run(_ context.Context, in CheckRunInput) {
	f.calls.Add(1)
	f.lastRepo = in.RepoID
	f.lastSHA = in.GitSHA
	f.lastN = len(in.FilePaths)
}

// TestPromote_InvokesCheckRunnerPostCommit verifies that when a CheckRunner is
// installed, Promote calls Run() after the tx commits.
func TestPromote_InvokesCheckRunnerPostCommit(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	n, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n}, nil)

	p := NewPromoter(sa, db)
	fr := &fakeCheckRunner{}
	p.SetCheckRunner(fr)

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := fr.calls.Load(); got != 1 {
		t.Fatalf("CheckRunner calls = %d, want 1", got)
	}
	if fr.lastRepo != "repo1" {
		t.Errorf("repoID = %q, want repo1", fr.lastRepo)
	}
	if fr.lastSHA != "sha-xyz" {
		t.Errorf("gitSHA = %q, want sha-xyz", fr.lastSHA)
	}
	if fr.lastN != 1 {
		t.Errorf("file count = %d, want 1", fr.lastN)
	}

	// Sanity: the tx still committed.
	if got := countNodes(t, db); got != 1 {
		t.Errorf("nodes: want 1, got %d", got)
	}
}

// TestPromote_NoCheckRunner verifies a regression guard: with no runner installed
// Promote behaves identically to the existing semantics.
func TestPromote_NoCheckRunner(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	n, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n}, nil)

	p := NewPromoter(sa, db)
	// Intentionally do not call SetCheckRunner.

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := countNodes(t, db); got != 1 {
		t.Errorf("nodes: want 1, got %d", got)
	}
}

// TestPromote_CheckRunnerSkippedWhenNothingStaged verifies the runner is NOT
// invoked when there is nothing to promote (early-return path before the tx).
func TestPromote_CheckRunnerSkippedWhenNothingStaged(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	p := NewPromoter(NewStagingArea(), db)
	fr := &fakeCheckRunner{}
	p.SetCheckRunner(fr)

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := fr.calls.Load(); got != 0 {
		t.Errorf("CheckRunner calls (empty staging) = %d, want 0", got)
	}
}
