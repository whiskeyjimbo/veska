// SPDX-License-Identifier: AGPL-3.0-only

package crossrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type fakeRepos struct {
	repos []RepoView
	err   error
}

func (f *fakeRepos) ListRepos(_ context.Context) ([]RepoView, error) {
	return f.repos, f.err
}

func TestNew_RejectsNilDeps(t *testing.T) {
	if _, err := New(nil, func(context.Context, string, string, string) ([]*domain.Node, error) { return nil, nil }); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("expected ErrMissingDependency, got %v", err)
	}
	if _, err := New(&fakeRepos{}, nil); !errors.Is(err, ErrMissingDependency) {
		t.Errorf("expected ErrMissingDependency, got %v", err)
	}
}

// TestLookupSymbol_PerRepoHitCounts pins the resolver's primary
// contract: every repo with at least one match yields a RepoMatch with
// the right HitCount, empty repos are omitted, and registry order is
// preserved.
func TestLookupSymbol_PerRepoHitCounts(t *testing.T) {
	repos := &fakeRepos{repos: []RepoView{
		{RepoID: "rA", RootPath: "/a", ActiveBranch: "main"},
		{RepoID: "rB", RootPath: "/b", ActiveBranch: "main"},
		{RepoID: "rC", RootPath: "/c", ActiveBranch: "main"},
	}}
	lookup := func(_ context.Context, repoID, _, _ string) ([]*domain.Node, error) {
		switch repoID {
		case "rA":
			return nil, nil // miss
		case "rB":
			return []*domain.Node{{}, {}}, nil // 2 hits
		case "rC":
			return []*domain.Node{{}}, nil // 1 hit
		}
		return nil, nil
	}
	r, err := New(repos, lookup)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := r.LookupSymbol(context.Background(), "Greeter")
	if err != nil {
		t.Fatalf("LookupSymbol: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches (rB, rC), got %d: %+v", len(got), got)
	}
	if got[0].RepoID != "rB" || got[0].HitCount != 2 {
		t.Errorf("first match wrong: got %+v", got[0])
	}
	if got[1].RepoID != "rC" || got[1].HitCount != 1 {
		t.Errorf("second match wrong: got %+v", got[1])
	}
}

// TestLookupSymbol_EmptyRegistry pins the empty-result case so callers
// that distinguish 'no matches anywhere' from 'matches elsewhere' get a
// length-zero slice and a nil error.
func TestLookupSymbol_EmptyRegistry(t *testing.T) {
	repos := &fakeRepos{repos: nil}
	lookup := func(context.Context, string, string, string) ([]*domain.Node, error) {
		return nil, errors.New("must not be called")
	}
	r, _ := New(repos, lookup)
	got, err := r.LookupSymbol(context.Background(), "X")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero matches, got %+v", got)
	}
}

// TestLookupSymbol_PerRepoErrorIsNonFatal pins the resilience contract: a
// single failing repo must not suppress the cross-repo hint from healthy
// repos; the error surfaces in the returned error chain so callers that
// care (doctor) can still see it.
func TestLookupSymbol_PerRepoErrorIsNonFatal(t *testing.T) {
	repos := &fakeRepos{repos: []RepoView{
		{RepoID: "broken", ActiveBranch: "main"},
		{RepoID: "ok", ActiveBranch: "main"},
	}}
	stuck := errors.New("disk read failed")
	lookup := func(_ context.Context, repoID, _, _ string) ([]*domain.Node, error) {
		if repoID == "broken" {
			return nil, stuck
		}
		return []*domain.Node{{}}, nil
	}
	r, _ := New(repos, lookup)
	got, err := r.LookupSymbol(context.Background(), "X")
	if err == nil || !errors.Is(err, stuck) {
		t.Errorf("expected wrapped stuck error, got %v", err)
	}
	if len(got) != 1 || got[0].RepoID != "ok" {
		t.Errorf("expected single match for ok repo, got %+v", got)
	}
}

// TestLookupSymbol_DefaultsEmptyBranchToMain pins the convenience: a
// repo without an active_branch should still be probed on "main".
func TestLookupSymbol_DefaultsEmptyBranchToMain(t *testing.T) {
	var sawBranch string
	repos := &fakeRepos{repos: []RepoView{{RepoID: "r", ActiveBranch: ""}}}
	lookup := func(_ context.Context, _, branch, _ string) ([]*domain.Node, error) {
		sawBranch = branch
		return []*domain.Node{{}}, nil
	}
	r, _ := New(repos, lookup)
	if _, err := r.LookupSymbol(context.Background(), "X"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sawBranch != "main" {
		t.Errorf("expected branch=main, got %q", sawBranch)
	}
}

// TestLookupSymbol_RejectsEmptyName pins the input-validation guard.
func TestLookupSymbol_RejectsEmptyName(t *testing.T) {
	r, _ := New(&fakeRepos{}, func(context.Context, string, string, string) ([]*domain.Node, error) { return nil, nil })
	if _, err := r.LookupSymbol(context.Background(), ""); err == nil {
		t.Errorf("expected error for empty symbolName, got nil")
	}
}
