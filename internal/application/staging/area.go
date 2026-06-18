// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package staging holds the in-memory, pre-promotion overlay: parse results
// that have been staged on save but not yet flushed to durable storage, plus
// the branch-switch quiescence gate that guards them. The application layer
// reads this overlay before SQLite so staged state is visible without a
// round-trip to durable storage; the Promoter drains it on commit.
package staging

import (
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// key is the composite key used to look up staged file state.
type key struct {
	repoID   string
	branch   string
	filePath string
}

// entry holds the nodes, edges, and unresolved-call markers for a
// single staged file. The unresolved markers are parser hints whose
// target lives in another file of the same package; the Promoter binds
// them at promotion time.
type entry struct {
	nodes      []*domain.Node
	edges      []*domain.Edge
	unresolved []domain.UnresolvedCall
	imports    map[string]string
}

// Area is a thread-safe, in-memory store of pending (not-yet-promoted)
// parse results keyed by (repoID, branch, filePath).
// It is intentionally lossy: constructing a new Area always produces
// empty state. Nothing is persisted to disk, so a daemon restart clears all
// staged data while promoted (SQLite) state survives unchanged.
// Overlay reads: callers check staging before hitting SQLite, so staged state
// is immediately visible without a round-trip to durable storage.
type Area struct {
	mu      sync.RWMutex
	entries map[key]entry
}

// NewArea constructs an empty Area.
func NewArea() *Area {
	return &Area{
		entries: make(map[key]entry),
	}
}

// config accumulates the optional behaviour of a Stage call.
type config struct {
	guard bool
	gen   uint64
	gate  *Gate
}

// Option configures a Stage call. The zero set of options is the common
// case: an unconditional overwrite of the file's staged parse data.
type Option func(*config)

// WithGenerationGuard makes Stage conditional on the staging generation: the
// write is discarded (Stage returns false) when gen no longer matches the
// gate's current generation. The ingest hot path uses it so in-flight saves
// from a branch that has since been switched away cannot pollute the new
// branch's staging.
func WithGenerationGuard(gen uint64, gate *Gate) Option {
	return func(c *config) {
		c.guard = true
		c.gen = gen
		c.gate = gate
	}
}

// Stage replaces all staged parse data for (repoID, branch, filePath) with f,
// overwriting any existing entry. It returns true when the data was staged.
// f carries whatever fidelity the caller has: a bare (Nodes, Edges) pair for
// tests and manual paths, or the full parser output (UnresolvedCalls + the
// import map) on the ingest path. Unset fields stage as nil.
// With WithGenerationGuard, Stage stages nothing and returns false when the
// supplied generation is stale relative to the gate ().
func (s *Area) Stage(repoID, branch, filePath string, f File, opts ...Option) bool {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.guard && cfg.gen != cfg.gate.Generation() {
		return false
	}
	k := key{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.Lock()
	s.entries[k] = entry{nodes: f.Nodes, edges: f.Edges, unresolved: f.UnresolvedCalls, imports: f.Imports}
	s.mu.Unlock()
	return true
}

// GetStagedNodes returns the staged nodes for (repoID, branch, filePath).
// Returns (nil, false) when no entry exists (cache miss - caller falls through
// to SQLite).
func (s *Area) GetStagedNodes(repoID, branch, filePath string) ([]*domain.Node, bool) {
	k := key{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.RLock()
	e, ok := s.entries[k]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.nodes, true
}

// GetStagedEdges returns the staged edges for (repoID, branch, filePath).
// Returns (nil, false) when no entry exists.
func (s *Area) GetStagedEdges(repoID, branch, filePath string) ([]*domain.Edge, bool) {
	k := key{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.RLock()
	e, ok := s.entries[k]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.edges, true
}

// DeleteStagedFile removes the staging entry for (repoID, branch, filePath).
// It is a no-op when the entry does not exist. Called after promotion to SQLite.
func (s *Area) DeleteStagedFile(repoID, branch, filePath string) {
	k := key{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.Lock()
	delete(s.entries, k)
	s.mu.Unlock()
}

// StagedFiles returns the file paths staged for (repoID, branch). The returned
// slice is always non-nil; it is empty when nothing is staged.
func (s *Area) StagedFiles(repoID, branch string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	paths := make([]string, 0)
	for k := range s.entries {
		if k.repoID == repoID && k.branch == branch {
			paths = append(paths, k.filePath)
		}
	}
	return paths
}

// Clear removes all staged state for (repoID, branch). Called on branch-switch
// to prevent stale overlay reads after the working tree changes.
func (s *Area) Clear(repoID, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k := range s.entries {
		if k.repoID == repoID && k.branch == branch {
			delete(s.entries, k)
		}
	}
}

// File is the per-file snapshot the promotion path consumes - nodes
// AND parser-produced edges. SIMILAR_TO edges (autolink) are NOT included
// here; only structural edges the parser determined at parse time
type File struct {
	Nodes           []*domain.Node
	Edges           []*domain.Edge
	UnresolvedCalls []domain.UnresolvedCall
	Imports         map[string]string
}

// Snapshot returns a shallow copy of staged nodes + edges keyed by filePath
// for the given (repoID, branch). Mutating the returned map does not affect
// the Area; the slices themselves are not deep-copied (callers must
// not mutate elements). Used by the promotion path to flush all staged
// state to SQLite in a single transaction.
// Parser-produced edges (CALLS, IMPORTS, etc.) ride with their file's
// nodes. Post-promotion SIMILAR_TO edges are produced separately by the
// autolink queue worker.
func (s *Area) Snapshot(repoID, branch string) map[string]File {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := make(map[string]File)
	for k, e := range s.entries {
		if k.repoID == repoID && k.branch == branch {
			ns := make([]*domain.Node, len(e.nodes))
			copy(ns, e.nodes)
			es := make([]*domain.Edge, len(e.edges))
			copy(es, e.edges)
			us := make([]domain.UnresolvedCall, len(e.unresolved))
			copy(us, e.unresolved)
			snap[k.filePath] = File{Nodes: ns, Edges: es, UnresolvedCalls: us, Imports: e.imports}
		}
	}
	return snap
}
