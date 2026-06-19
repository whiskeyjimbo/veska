// SPDX-License-Identifier: AGPL-3.0-only

package application_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// fakeCheckRunner records each invocation and the input it received.
type fakeCheckRunner struct {
	calls     atomic.Int32
	lastRepo  string
	lastSHA   string
	lastN     int
	lastAdded map[string][]application.Line
}

func (f *fakeCheckRunner) Run(_ context.Context, in application.CheckRunInput) {
	f.calls.Add(1)
	f.lastRepo = in.RepoID
	f.lastSHA = in.GitSHA
	f.lastN = len(in.FilePaths)
	f.lastAdded = in.AddedLines
}

// TestPromote_InvokesCheckRunnerPostCommit verifies that when a CheckRunner is
// installed, Promote calls Run after the tx commits.
func TestPromote_InvokesCheckRunnerPostCommit(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	fr := &fakeCheckRunner{}
	p := newTestPromoter(sa, db, application.WithCheckRunner(fr))

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

// TestPromote_PopulatesAddedLinesFromSeam verifies that when an
// AddedLinesFunc seam is installed, Promote calls it for the promoted
// commit and forwards the resulting per-file added lines on CheckRunInput.
func TestPromote_PopulatesAddedLinesFromSeam(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	want := map[string][]application.Line{
		"a.go": {{Number: 1, Text: "package a"}, {Number: 2, Text: "func A() {}"}},
	}
	var gotRepo, gotSHA string
	fr := &fakeCheckRunner{}
	p := newTestPromoter(sa, db,
		application.WithCheckRunner(fr),
		application.WithAddedLinesFunc(func(_ context.Context, repoID, gitSHA string) (map[string][]application.Line, error) {
			gotRepo, gotSHA = repoID, gitSHA
			return want, nil
		}),
	)

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if gotRepo != "repo1" || gotSHA != "sha-xyz" {
		t.Errorf("AddedLinesFunc args = (%q,%q), want (repo1,sha-xyz)", gotRepo, gotSHA)
	}
	got := fr.lastAdded["a.go"]
	if len(got) != 2 {
		t.Fatalf("AddedLines[a.go] = %v, want 2 lines", got)
	}
	if got[0] != (application.Line{Number: 1, Text: "package a"}) ||
		got[1] != (application.Line{Number: 2, Text: "func A() {}"}) {
		t.Errorf("AddedLines[a.go] = %+v, want %+v", got, want["a.go"])
	}
}

// TestPromote_NoAddedLinesFunc verifies that without the seam installed
// AddedLines is simply nil and Promote behaves unchanged.
func TestPromote_NoAddedLinesFunc(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	fr := &fakeCheckRunner{}
	p := newTestPromoter(sa, db, application.WithCheckRunner(fr))

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if fr.lastAdded != nil {
		t.Errorf("AddedLines = %v, want nil", fr.lastAdded)
	}
}

// TestPromote_NoCheckRunner verifies a regression guard: with no runner installed
// Promote behaves identically to the existing semantics.
func TestPromote_NoCheckRunner(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n}, Edges: nil})

	p := newTestPromoter(sa, db)
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

	fr := &fakeCheckRunner{}
	p := newTestPromoter(staging.NewArea(), db, application.WithCheckRunner(fr))

	if err := p.Promote(context.Background(), "repo1", "main", "sha-xyz",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := fr.calls.Load(); got != 0 {
		t.Errorf("CheckRunner calls (empty staging) = %d, want 0", got)
	}
}
