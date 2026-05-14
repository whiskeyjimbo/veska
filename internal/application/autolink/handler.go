package autolink

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// candidateProducer is the minimal contract the Handler needs from the
// Linker so the handler can be tested without spinning up the embedding
// + vector stack. *Linker satisfies this interface structurally.
type candidateProducer interface {
	Candidates(ctx context.Context, repoID, branch string, sourceNodeIDs []string) ([]Candidate, error)
}

// fileNodeLookup is the narrow port the Handler needs from ports.NodeLookup.
// Defined here so the autolink package does not import the full NodeLookup
// surface (which carries the LookupNodes method aimed at the search layer).
type fileNodeLookup interface {
	NodesInFile(ctx context.Context, repoID, branch, filePath string) ([]string, error)
}

// Handler implements queue.WorkHandler for WorkKindAutoLink rows.
//
// One Row -> one batch of unresolved Edges + one Finding per Edge:
//
//  1. Validate row.Kind == WorkKindAutoLink.
//  2. Resolve the payload file path to its set of source node_ids.
//  3. Ask the Linker for top-k similarity candidates across those sources.
//  4. Persist each candidate as a SIMILAR_TO edge with Confidence=Unresolved.
//  5. Persist one source_layer='semantic' Finding per candidate, anchored
//     on the edge_id (stored in the findings.node_id TEXT column, which is
//     intentionally schemaless wrt foreign keys at the SQL level).
//
// Idempotency: EdgeStorage uses ON CONFLICT DO NOTHING; FindingStorage uses
// ON CONFLICT DO UPDATE on a finding_id derived from (rule + anchor). Both
// paths handle re-delivery from the queue.Poller without duplication.
type Handler struct {
	linker   candidateProducer
	lookup   fileNodeLookup
	edges    ports.EdgeStorage
	findings ports.FindingStorage
}

// HandlerOption configures a Handler. None are required today; the type is
// here so future cross-cutting concerns (metrics, clocks) can land without a
// breaking constructor change.
type HandlerOption func(*Handler)

// NewHandler constructs a Handler. All four collaborators are required;
// nil arguments are programmer errors and reported by panicking at
// construction time, mirroring the NewLinker contract in this package.
func NewHandler(
	linker candidateProducer,
	lookup fileNodeLookup,
	edges ports.EdgeStorage,
	findings ports.FindingStorage,
	opts ...HandlerOption,
) *Handler {
	if linker == nil {
		panic("autolink.NewHandler: linker is nil")
	}
	if lookup == nil {
		panic("autolink.NewHandler: lookup is nil")
	}
	if edges == nil {
		panic("autolink.NewHandler: edges is nil")
	}
	if findings == nil {
		panic("autolink.NewHandler: findings is nil")
	}
	h := &Handler{
		linker:   linker,
		lookup:   lookup,
		edges:    edges,
		findings: findings,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Rule is the finding rule emitted by the auto-link handler. Exposed so
// tests and other tooling (suppressions, dashboards) can reference it
// without re-hard-coding the string.
const Rule = "auto-link"

// Handle processes a single ports.WorkRow of kind WorkKindAutoLink.
//
// Behaviour:
//   - Wrong kind returns an error (programmer or routing bug).
//   - Empty payload returns nil (nothing to do).
//   - File with zero nodes is a no-op.
//   - Linker / EdgeStorage / FindingStorage errors propagate wrapped so the
//     queue.Poller can re-queue or mark the row failed.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindAutoLink {
		return fmt.Errorf("autolink.Handle: unexpected kind %q", row.Kind)
	}
	filePath := row.Payload
	if filePath == "" {
		return nil
	}

	nodeIDs, err := h.lookup.NodesInFile(ctx, row.RepoID, row.Branch, filePath)
	if err != nil {
		return fmt.Errorf("autolink.Handle: nodes in file %q: %w", filePath, err)
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	cands, err := h.linker.Candidates(ctx, row.RepoID, row.Branch, nodeIDs)
	if err != nil {
		return fmt.Errorf("autolink.Handle: linker: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}

	edges := make([]*domain.Edge, 0, len(cands))
	for _, c := range cands {
		e, err := domain.NewEdge(
			domain.NodeID(c.SourceNodeID),
			domain.NodeID(c.TargetNodeID),
			domain.EdgeSimilarTo,
			domain.WithConfidence(domain.Unresolved),
		)
		if err != nil {
			return fmt.Errorf("autolink.Handle: build edge: %w", err)
		}
		edges = append(edges, e)
	}

	if err := h.edges.SaveEdges(ctx, row.RepoID, row.Branch, edges); err != nil {
		return fmt.Errorf("autolink.Handle: save edges: %w", err)
	}

	for i, c := range cands {
		e := edges[i]
		// Anchor the finding on the edge_id (opaque TEXT in findings.node_id).
		// This makes (rule, anchor) unique per candidate edge, so finding_id
		// is unique per candidate and the ON CONFLICT(finding_id, branch)
		// idempotency in FindingRepo applies cleanly on re-delivery.
		f, err := domain.NewFinding(
			"", // ID intentionally empty: branch-stable finding_id is computed inside NewFinding.
			row.RepoID,
			row.Branch,
			domain.SeverityLow,
			domain.LayerSemantic,
			Rule,
			fmt.Sprintf("Similar to %s (score %.2f)", c.TargetNodeID, c.Score),
			domain.WithNodeAnchor(e.ID),
		)
		if err != nil {
			return fmt.Errorf("autolink.Handle: build finding: %w", err)
		}
		if err := h.findings.Save(ctx, f); err != nil {
			return fmt.Errorf("autolink.Handle: save finding: %w", err)
		}
	}

	return nil
}

// Compile-time check that *Handler satisfies ports.WorkHandler (and, by
// type alias, the historical infrastructure/sqlite/queue.WorkHandler).
var _ ports.WorkHandler = (*Handler)(nil)
