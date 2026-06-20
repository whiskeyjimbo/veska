// SPDX-License-Identifier: AGPL-3.0-only

// Package coverage builds the node→test reverse map: for a
// given prod node, the set of runnable test entrypoint functions that
// transitively call it over CALLS edges. It needs no new ingestion - the
// signal is latent in the CALLS edges already in the graph (the same proxy the
// untested-symbol check consumes, extended from direct presence to a transitive
// function-granularity map).
// The map is the data; impact-based test SELECTION is the
// consumer that turns it into a `go test -run` set. Both stay producer-side
// no test execution, no real coverage ingestion.
package coverage

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by NewReverseMap when a required dependency
// is nil, matching the application-service convention.
var ErrMissingDependency = errors.New("coverage: missing required dependency")

// DefaultMaxNodes bounds the inbound BFS by total visited nodes - a safety
// valve against a pathological reverse-reachability fan-out, NOT a precision
// knob. It is deliberately generous: for test SELECTION the safe failure is
// OVER-selecting (running a few extra tests), so the walk uses unbounded depth
// and no hub-gating (unlike blastradius, whose shallow depth + hub gate would
// silently DROP covering tests). Decision B.
const DefaultMaxNodes = 5000

// InboundCallsReader is the CALLS-scoped, metadata-bearing inbound adjacency
// the reverse-map BFS walks. It is consumer-owned (only this package needs it)
// and sized to exactly one method: neither ports.EdgeReader (kind-agnostic, so
// it would pollute the walk with CONTAINS/SIMILAR_TO edges) nor
// ports.CoverageQuerier (direct-only, file-granularity) fits. sqlite.CoverageRepo
// satisfies it.
type InboundCallsReader interface {
	// InboundCallsEdges returns, for each node_id in nodeIDs, the source nodes
	// of its inbound CALLS edges in (repoID, branch), carrying enough metadata
	// (kind, name, file) for the application layer to classify test entrypoints.
	// A node with no inbound CALLS caller maps to an empty/nil slice.
	InboundCallsEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]ports.NodeRef, error)
}

// TestRef identifies a runnable test entrypoint function that covers a prod
// node. Name is the bare function name (nodes.symbol_path for a function),
// which is exactly the token `go test -run` matches.
type TestRef struct {
	NodeID   string
	Name     string
	FilePath string
}

// ReverseMap answers "which test functions transitively call this prod node?".
// It is stateless and safe for concurrent callers.
type ReverseMap struct {
	r        InboundCallsReader
	maxNodes int
}

// ReverseMapOption configures a ReverseMap at construction.
type ReverseMapOption func(*ReverseMap)

// WithMaxNodes overrides the BFS visited-node safety valve. A non-positive
// value restores DefaultMaxNodes.
func WithMaxNodes(n int) ReverseMapOption {
	return func(m *ReverseMap) {
		if n > 0 {
			m.maxNodes = n
		}
	}
}

// NewReverseMap constructs a ReverseMap bound to r. r is required; a nil
// dependency is reported with a wrapped ErrMissingDependency.
func NewReverseMap(r InboundCallsReader, opts ...ReverseMapOption) (*ReverseMap, error) {
	if r == nil {
		return nil, fmt.Errorf("coverage.NewReverseMap: reader is nil: %w", ErrMissingDependency)
	}
	m := &ReverseMap{r: r, maxNodes: DefaultMaxNodes}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// TestsCovering returns the runnable test entrypoint functions that
// transitively reach nodeID via inbound CALLS edges. A prod node with no test
// caller yields an empty slice (AC2: empty set, not an error). The result is
// deterministically ordered by first-discovery BFS order.
func (m *ReverseMap) TestsCovering(ctx context.Context, repoID, branch, nodeID string) ([]TestRef, error) {
	return m.TestsCoveringAny(ctx, repoID, branch, []string{nodeID})
}

// TestsCoveringAny returns the UNION of test entrypoints covering any seed node
// the form impact-based selection (v6de.2) needs over a diff's changed-node
// set. A single inbound BFS is shared across all seeds. Duplicate test
// entrypoints reached from multiple seeds are de-duplicated.
func (m *ReverseMap) TestsCoveringAny(ctx context.Context, repoID, branch string, seedNodeIDs []string) ([]TestRef, error) {
	if m == nil || m.r == nil {
		return nil, fmt.Errorf("coverage.TestsCoveringAny: nil reader")
	}

	visited := make(map[string]struct{}, len(seedNodeIDs))
	frontier := make([]string, 0, len(seedNodeIDs))
	for _, id := range seedNodeIDs {
		if id == "" {
			continue
		}
		if _, ok := visited[id]; !ok {
			visited[id] = struct{}{}
			frontier = append(frontier, id)
		}
	}

	var (
		tests   []TestRef
		seenTst = make(map[string]struct{})
	)

	for len(frontier) > 0 && len(visited) < m.maxNodes {
		callers, err := m.r.InboundCallsEdges(ctx, repoID, branch, frontier)
		if err != nil {
			return nil, fmt.Errorf("coverage.TestsCoveringAny: inbound calls: %w", err)
		}
		next := make([]string, 0, len(frontier))
		// Iterate the frontier (not the map) so traversal order is deterministic.
		for _, seed := range frontier {
			for _, c := range callers[seed] {
				if c.NodeID == "" {
					continue
				}
				if _, ok := visited[c.NodeID]; ok {
					continue
				}
				visited[c.NodeID] = struct{}{}
				if isTestEntrypoint(c) {
					// A test entrypoint is a leaf of the coverage walk: it
					// covers the seed, and we do NOT expand past it (a test
					// calling another test is not a coverage path we track).
					if _, dup := seenTst[c.NodeID]; !dup {
						seenTst[c.NodeID] = struct{}{}
						tests = append(tests, TestRef{NodeID: c.NodeID, Name: c.Name, FilePath: c.FilePath})
					}
					continue
				}
				// Helper or prod caller: keep walking inbound.
				next = append(next, c.NodeID)
			}
		}
		frontier = next
	}

	return tests, nil
}

// isTestEntrypoint reports whether ref is a runnable Go test entrypoint
// function - a top-level func in a *_test.go file whose name is a go-test
// recognized prefix (Test/Benchmark/Fuzz/Example). Helpers in test files are
// deliberately NOT entrypoints; they are walked THROUGH as intermediate hops
// ( granularity decision).
func isTestEntrypoint(ref ports.NodeRef) bool {
	if ref.Kind != "function" {
		return false
	}
	if !pathfilter.IsTestFile(ref.FilePath) {
		return false
	}
	return isGoTestName(ref.Name)
}

// isGoTestName applies go test's own naming rule: a recognized prefix
// (Test, Benchmark, Fuzz, Example) where the following rune, if any, is not a
// lowercase letter - so `TestFoo` matches but the helper `Testify` does not.
func isGoTestName(name string) bool {
	for _, prefix := range [...]string{"Test", "Benchmark", "Fuzz", "Example"} {
		if len(name) < len(prefix) || name[:len(prefix)] != prefix {
			continue
		}
		rest := name[len(prefix):]
		if rest == "" {
			return true // bare `func Test(t *testing.T)` is a valid test
		}
		c := rest[0]
		if c >= 'a' && c <= 'z' {
			return false
		}
		return true
	}
	return false
}
