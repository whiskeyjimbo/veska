package application

import (
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// stagingKey is the composite key used to look up staged file state.
type stagingKey struct {
	repoID   string
	branch   string
	filePath string
}

// stagedEntry holds the nodes and edges for a single staged file.
type stagedEntry struct {
	nodes []*domain.Node
	edges []*domain.Edge
}

// StagingArea is a thread-safe, in-memory store of pending (not-yet-promoted)
// parse results keyed by (repoID, branch, filePath).
//
// It is intentionally lossy: constructing a new StagingArea always produces
// empty state. Nothing is persisted to disk, so a daemon restart clears all
// staged data while promoted (SQLite) state survives unchanged.
//
// Overlay reads: callers check staging before hitting SQLite, so staged state
// is immediately visible without a round-trip to durable storage.
type StagingArea struct {
	mu      sync.RWMutex
	entries map[stagingKey]stagedEntry
}

// NewStagingArea constructs an empty StagingArea.
func NewStagingArea() *StagingArea {
	return &StagingArea{
		entries: make(map[stagingKey]stagedEntry),
	}
}

// StageFile replaces all staged nodes and edges for (repoID, branch, filePath).
// Calling this twice for the same key overwrites the first entry.
func (s *StagingArea) StageFile(repoID, branch, filePath string, nodes []*domain.Node, edges []*domain.Edge) {
	key := stagingKey{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.Lock()
	s.entries[key] = stagedEntry{nodes: nodes, edges: edges}
	s.mu.Unlock()
}

// GetStagedNodes returns the staged nodes for (repoID, branch, filePath).
// Returns (nil, false) when no entry exists (cache miss — caller falls through
// to SQLite).
func (s *StagingArea) GetStagedNodes(repoID, branch, filePath string) ([]*domain.Node, bool) {
	key := stagingKey{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.nodes, true
}

// GetStagedEdges returns the staged edges for (repoID, branch, filePath).
// Returns (nil, false) when no entry exists.
func (s *StagingArea) GetStagedEdges(repoID, branch, filePath string) ([]*domain.Edge, bool) {
	key := stagingKey{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return e.edges, true
}

// DeleteStagedFile removes the staging entry for (repoID, branch, filePath).
// It is a no-op when the entry does not exist. Called after promotion to SQLite.
func (s *StagingArea) DeleteStagedFile(repoID, branch, filePath string) {
	key := stagingKey{repoID: repoID, branch: branch, filePath: filePath}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// StagedFiles returns the file paths staged for (repoID, branch). The returned
// slice is always non-nil; it is empty when nothing is staged.
func (s *StagingArea) StagedFiles(repoID, branch string) []string {
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
func (s *StagingArea) Clear(repoID, branch string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k := range s.entries {
		if k.repoID == repoID && k.branch == branch {
			delete(s.entries, k)
		}
	}
}

// StageIfCurrentGeneration stages the file only if gen matches the gate's current
// generation. Returns false and discards the write if the generation is stale.
// This prevents in-flight saves from a prior branch polluting the new branch's staging.
func (s *StagingArea) StageIfCurrentGeneration(
	repoID, branch, filePath string,
	nodes []*domain.Node,
	edges []*domain.Edge,
	gen uint64,
	gate *IngestionGate,
) bool {
	if gen != gate.Generation() {
		return false
	}
	s.StageFile(repoID, branch, filePath, nodes, edges)
	return true
}

// Snapshot returns a shallow copy of staged nodes keyed by filePath for the
// given (repoID, branch). Mutating the returned map does not affect the
// StagingArea; however, the node slices themselves are not deep-copied (callers
// must not mutate the slice elements). Used by the promotion path to flush all
// staged state to SQLite in a single transaction.
//
// Edges are intentionally excluded from the snapshot. Promotion is a node-only
// operation; edges are re-derived post-promotion by the auto_link queue worker
// (work_kind="auto_link" enqueued by Promoter). Staged edges are retained in
// the StagingArea only to serve pre-promotion overlay reads via GetStagedEdges.
func (s *StagingArea) Snapshot(repoID, branch string) map[string][]*domain.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := make(map[string][]*domain.Node)
	for k, e := range s.entries {
		if k.repoID == repoID && k.branch == branch {
			// Shallow-copy the slice so callers can append/delete without
			// affecting the staging store's slice header.
			ns := make([]*domain.Node, len(e.nodes))
			copy(ns, e.nodes)
			snap[k.filePath] = ns
		}
	}
	return snap
}
