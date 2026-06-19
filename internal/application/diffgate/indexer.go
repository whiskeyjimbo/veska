// SPDX-License-Identifier: AGPL-3.0-only

// Package diffgate is the shared substrate of the diff-safety gate
// it ephemerally indexes a candidate change against a base
// graph WITHOUT mutating the persisted graph or hitting the network, so the
// verify and blast-radius-containment
// verdicts can query a (before, after) graph state. It computes no verdicts
// itself - those are the consumer tasks; this package only produces the
// queryable substrate they share.
// Two seams keep the substrate stable while its inputs vary:
//
//	ChangeSource (the "after" input): where the candidate comes from. v1 is
//	  a git ref/worktree (RefChangeSource); a raw unified-diff source is the
//	  deferred. Verify/guard never see it.
//	BaseGraph (the "before" input): what the candidate is diffed against.
//	  v1 is the persisted, indexed-HEAD graph (SQLite already implements
//	  EdgeReader+NodeLookup). An in-memory build at an arbitrary base ref, or
//	  a version-pinned query, are future backends behind the
//	  same interface - so neither the Indexer nor the consumers change.
//
// Nothing here is persisted and no EmbeddingProvider is touched, so indexing
// adds no network egress (AC2).
package diffgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by constructors when a required dependency
// is nil/empty. It wraps so callers can errors.Is against it.
var ErrMissingDependency = errors.New("diffgate: missing required dependency")

// CallEdgeReader is the consumer-owned narrow port the dead-code resolution
// predicate needs from the base: CALLS-only inbound adjacency. It is distinct
// from EdgeReader.InboundEdges (all kinds, used by the blast-radius BFS) so the
// liveness re-run agrees with the CALLS-only check that raised the finding
// counting structural CONTAINS edges resolved every dead-code finding for free
type CallEdgeReader interface {
	// InboundCallEdges returns, for each node_id, the src_node_id values of its
	// inbound CALLS edges only.
	InboundCallEdges(ctx context.Context, repoID, branch string, nodeIDs []string) (map[string][]string, error)
}

// BaseGraph is the "before" picture the candidate is diffed against: a
// queryable graph exposing the same adjacency (EdgeReader) and node-metadata
// (NodeLookup) ports the gate's consumers already use, plus CALLS-only inbound
// (CallEdgeReader) for dead-code liveness. v1 is the persisted indexed-HEAD
// graph; future backends (in-memory parse-at-ref, 9fsn pinned query) satisfy
// the same interface so verify/guard never change.
type BaseGraph interface {
	ports.EdgeReader
	ports.NodeLookup
	CallEdgeReader
}

// Ephemeral is the (base, candidate) graph state the Indexer produces. Base is
// the unchanged "before"; Overlay holds the candidate's parsed nodes/edges as
// a staging overlay (a deleted file stages an empty entry). Consumers compose
// the two - the gate's blast-radius guard, for instance, queries
// blastradius.NewService(eph.Base, eph.Base, eph.Overlay) with no new
// interface - and read the overlay shadowing the base on the changed files.
// Building an Ephemeral mutates no durable state: Base is read-only and
// Overlay is a fresh in-memory staging.Area.
type Ephemeral struct {
	Base    BaseGraph
	Overlay *staging.Area
	RepoID  string
	Branch  string
	// ChangedFiles is the set of repo-relative paths the candidate touched
	// (added, modified, or deleted), in ChangeSource order.
	ChangedFiles []string
}

// Indexer parses a ChangeSource's files into a candidate overlay and pairs it
// with a BaseGraph to produce an Ephemeral. It is stateless and safe for
// concurrent callers.
type Indexer struct {
	parser ports.CodeParser
}

// NewIndexer constructs an Indexer. The parser is required.
func NewIndexer(parser ports.CodeParser) (*Indexer, error) {
	if parser == nil {
		return nil, fmt.Errorf("%w: parser is nil", ErrMissingDependency)
	}
	return &Indexer{parser: parser}, nil
}

// Index pulls the candidate change from src, parses each changed file into the
// overlay, and returns the Ephemeral pairing that overlay with base. The base
// graph is scoped to (repoID, branch); the overlay is staged under the same
// scope so consumers read both with one (repoID, branch). No network egress
// occurs: only the injected parser and the in-memory overlay are touched.
func (ix *Indexer) Index(ctx context.Context, repoID, branch string, base BaseGraph, src ChangeSource) (*Ephemeral, error) {
	if base == nil {
		return nil, fmt.Errorf("%w: base graph is nil", ErrMissingDependency)
	}
	if src == nil {
		return nil, fmt.Errorf("%w: change source is nil", ErrMissingDependency)
	}
	changes, err := src.Changes(ctx)
	if err != nil {
		return nil, fmt.Errorf("diffgate: read change source: %w", err)
	}
	overlay := staging.NewArea()
	changedFiles := make([]string, 0, len(changes))
	for _, fc := range changes {
		if fc.Deleted {
			// Stage an empty entry so the overlay records the file as
			// present-with-no-symbols, shadowing the base's nodes for it.
			overlay.Stage(repoID, branch, fc.Path, staging.File{})
			changedFiles = append(changedFiles, fc.Path)
			continue
		}
		pr, err := ix.parser.ParseFile(ctx, repoID, fc.Path, fc.Content)
		if err != nil {
			return nil, fmt.Errorf("diffgate: parse %s: %w", fc.Path, err)
		}
		overlay.Stage(repoID, branch, fc.Path, staging.File{
			Nodes:           pr.Nodes,
			Edges:           pr.Edges,
			UnresolvedCalls: pr.UnresolvedCalls,
			Imports:         pr.Imports,
		})
		changedFiles = append(changedFiles, fc.Path)
	}
	return &Ephemeral{
		Base:         base,
		Overlay:      overlay,
		RepoID:       repoID,
		Branch:       branch,
		ChangedFiles: changedFiles,
	}, nil
}
