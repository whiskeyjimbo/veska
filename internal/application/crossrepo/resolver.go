// Package crossrepo provides a small primitive that walks every registered
// repository and aggregates per-repo hit counts for a given symbol or
// finding query .
//
// Today each surface (`veska symbol`, `veska findings list`, `veska search`)
// is one-repo-scoped — a junior on repo A who runs `veska symbol Greeter`
// gets "no matches" with no hint that Greeter is defined in repo B. This
// resolver gives those surfaces one shared way to ask "where does this
// symbol live across the registry?" so they can format consistent hints
// ("no matches in A; 1 in B"). The resolver itself does not change any
// CLI output — that lives in the surfaces (solov2-zgwd / 0vau / vm5w).
package crossrepo

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ErrMissingDependency is returned from constructors when a required
// collaborator is nil. Mirrors the sentinel convention used elsewhere in
// the codebase (no panics on misconfiguration).
var ErrMissingDependency = errors.New("crossrepo: required dependency is nil")

// RepoView is the minimal repo identity the resolver needs. The repo
// registry (internal/repo) returns richer Records; we copy only what the
// resolver and its callers actually use to keep this package's deps small.
type RepoView struct {
	RepoID       string
	RootPath     string
	ActiveBranch string
}

// RepoLister enumerates the registered repos. Backed by repo.List in
// production; a test fake here covers the empty-registry case.
type RepoLister interface {
	ListRepos(ctx context.Context) ([]RepoView, error)
}

// SymbolLookup finds Nodes whose name matches symbolName in a specific
// (repoID, branch). It is the same FindNodes contract ports.GraphStorage
// exposes; we accept the narrower function shape so the resolver does not
// pull GraphStorage's full interface into application-level wiring.
type SymbolLookup func(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error)

// RepoMatch is one row of LookupSymbol's response: how many nodes in this
// repo match the queried symbol, plus the repo identity for hint formatting.
type RepoMatch struct {
	RepoID       string
	RootPath     string
	ActiveBranch string
	HitCount     int
}

// Resolver coordinates a multi-repo symbol probe. Construction takes the
// list-and-lookup pair so callers can swap in fakes — the daemon wires a
// real repo registry + ports.GraphStorage.FindNodes; tests use stubs.
type Resolver struct {
	repos  RepoLister
	lookup SymbolLookup
}

// New constructs a Resolver. Both repos and lookup are required; a nil
// returns ErrMissingDependency rather than panicking on the first call.
func New(repos RepoLister, lookup SymbolLookup) (*Resolver, error) {
	if repos == nil || lookup == nil {
		return nil, fmt.Errorf("%w (repos and lookup must be non-nil)", ErrMissingDependency)
	}
	return &Resolver{repos: repos, lookup: lookup}, nil
}

// LookupSymbol queries every registered repo for symbolName on its active
// branch and returns a RepoMatch for each repo that produced at least one
// hit. Repos with no matches are omitted so callers can decide between "no
// matches anywhere" (empty slice) and "matches in other repos" by length
// alone. The slice preserves repo-registry order so callers see a stable
// listing across invocations.
//
// Per-repo lookup errors are NOT fatal — one stuck repo would otherwise
// suppress the entire cross-repo hint. They are returned in the second
// return value via errors.Join so a caller that wants to surface them
// (e.g. doctor) still has access; surfaces that only render hints can
// safely ignore the error.
func (r *Resolver) LookupSymbol(ctx context.Context, symbolName string) ([]RepoMatch, error) {
	if symbolName == "" {
		return nil, fmt.Errorf("crossrepo: empty symbolName")
	}
	repos, err := r.repos.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("crossrepo: list repos: %w", err)
	}
	var matches []RepoMatch
	var errs []error
	for _, rv := range repos {
		branch := rv.ActiveBranch
		if branch == "" {
			branch = "main"
		}
		hits, lerr := r.lookup(ctx, rv.RepoID, branch, symbolName)
		if lerr != nil {
			errs = append(errs, fmt.Errorf("crossrepo: %s: %w", rv.RepoID, lerr))
			continue
		}
		if len(hits) == 0 {
			continue
		}
		matches = append(matches, RepoMatch{
			RepoID:       rv.RepoID,
			RootPath:     rv.RootPath,
			ActiveBranch: branch,
			HitCount:     len(hits),
		})
	}
	return matches, errors.Join(errs...)
}

// ListRepos is a thin pass-through to the underlying registry, exposed so
// callers (notably the findings --all surface, solov2-0vau) can enumerate
// without keeping a second reference to the repo registry. It is the same
// data ListRepos returns from RepoLister; we forward it verbatim.
func (r *Resolver) ListRepos(ctx context.Context) ([]RepoView, error) {
	return r.repos.ListRepos(ctx)
}
