// Package autolink computes auto-link candidates: top-k similarity edges
// proposed for a set of recently-promoted source nodes.
//
// This package ships the pure computation only (m3.04.1). It does not write
// findings or unresolved edges (m3.04.2) and is not wired into the queue
// handler (m3.04.3). The Linker reads embedding bytes via a narrow port
// (EmbeddingLookup) and queries a VectorStorage for nearest neighbours; the
// result is a flat slice of Candidate rows ready for downstream consumption.
//
// Score direction. Hit.Score from ports.VectorStorage.Search is always a
// "higher = closer" similarity: both active backends (sqlite-vec and
// usearch) compute L2-squared distance internally and report
// score = 1 / (1 + dist). The threshold is therefore a simple lower bound
// on Hit.Score; no per-backend normalisation is required at this layer.
//
// Score range depends on input. score lands in (0, 1] only when stored
// embeddings are unit-length (L2-squared distance then bounded in [0, 4]).
// The embedder pipeline L2-normalises every vector before storage
// precisely so this holds — see internal/application/embedder. If that
// invariant is ever broken, DefaultThreshold below becomes meaningless.
package autolink

import (
	"context"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

const (
	// DefaultTopK is the per-source candidate cap when no Option overrides it.
	DefaultTopK = 5

	// DefaultThreshold is the minimum Hit.Score for a candidate to be emitted.
	// Tuned against the gate-3 measurement (solov2-d5z / solov2-uug): on a
	// real nomic-embed-text fixture of unit-normalised vectors, within-topic
	// pairs score ≈0.66 and cross-topic pairs ≈0.50. 0.60 sits in that gap —
	// it admitted 100% of true links with a 0.00% false-positive rate on the
	// gate-3 fixture. (The original 0.85 assumed a cosine-like score range
	// and was unreachable once real embedding norms were accounted for.)
	DefaultThreshold float32 = 0.60
)

// Candidate is a single proposed auto-link edge. The Score is copied verbatim
// from the underlying VectorStorage.Hit and is in the score-space documented
// on the package (higher = more similar).
type Candidate struct {
	SourceNodeID string
	TargetNodeID string
	Score        float32
	RepoID       string
	Branch       string
}

// EmbeddingLookup is the narrow port the Linker needs from the embedding-refs
// adapter. It is intentionally smaller than ports.EmbeddingRefRepo so the
// autolink package depends only on what it uses; the concrete repo struct
// satisfies this interface structurally. Mirrors the m3.01.1 Promoter /
// CheckRunner pattern.
type EmbeddingLookup interface {
	// ContentHashForNode returns the content_hash of the embedding for the
	// given node, plus a ready flag.
	//
	//   ready=true, hash non-empty: the embedding bytes are available via
	//     LookupExisting(hash).
	//   ready=false: the node has no row in node_embedding_refs, the row is
	//     state='pending' (no hash yet), or state='failed'. The Linker treats
	//     all three as "skip this source, not an error".
	ContentHashForNode(ctx context.Context, repoID, branch, nodeID string) (contentHash string, ready bool, err error)

	// LookupExisting fetches the stored embedding BLOB and dimension for
	// contentHash. found=false with nil error means a row vanished between
	// ContentHashForNode and the lookup (eviction or DB inconsistency) — the
	// Linker treats this as "skip this source".
	LookupExisting(ctx context.Context, contentHash string) (embedding []byte, dim int, found bool, err error)
}

// Linker computes top-k similarity candidates for a set of source nodes. The
// zero value is not usable; construct with NewLinker.
type Linker struct {
	refs      EmbeddingLookup
	vectors   ports.VectorStorage
	k         int
	threshold float32
	metrics   *observability.Metrics
}

// Option configures a Linker.
type Option func(*Linker)

// WithTopK overrides the per-source candidate cap. Non-positive values are
// ignored (the default stands).
func WithTopK(k int) Option {
	return func(l *Linker) {
		if k > 0 {
			l.k = k
		}
	}
}

// WithThreshold overrides the minimum Hit.Score required to emit a candidate.
// Negative values are ignored; zero is allowed (admit every non-self hit).
func WithThreshold(t float32) Option {
	return func(l *Linker) {
		if t >= 0 {
			l.threshold = t
		}
	}
}

// WithMetrics installs a Metrics struct so Candidates increments
// AutolinkCandidates on every emitted candidate. nil disables observation.
func WithMetrics(m *observability.Metrics) Option {
	return func(l *Linker) { l.metrics = m }
}

// ErrMissingDependency is returned by the autolink constructors (NewLinker,
// NewHandler) when a required collaborator is nil. It is errors.Is-matchable
// so callers can distinguish a wiring fault from a runtime failure.
var ErrMissingDependency = errors.New("autolink: missing required dependency")

// NewLinker constructs a Linker. refs and vectors are required: a nil
// dependency yields an error wrapping ErrMissingDependency and a nil *Linker.
func NewLinker(refs EmbeddingLookup, vectors ports.VectorStorage, opts ...Option) (*Linker, error) {
	if refs == nil {
		return nil, fmt.Errorf("autolink.NewLinker: refs is nil: %w", ErrMissingDependency)
	}
	if vectors == nil {
		return nil, fmt.Errorf("autolink.NewLinker: vectors is nil: %w", ErrMissingDependency)
	}
	l := &Linker{
		refs:      refs,
		vectors:   vectors,
		k:         DefaultTopK,
		threshold: DefaultThreshold,
	}
	for _, o := range opts {
		o(l)
	}
	return l, nil
}

// Candidates computes top-k similarity candidates for every source node in
// sourceNodeIDs. The output is the union across sources, in input order; ties
// inside a single source are broken by VectorStorage.Search rank order.
//
// Per-source flow:
//  1. Look up the source node's content_hash via EmbeddingLookup. If the node
//     is not ready (pending, failed, missing), it is silently skipped — this
//     is best-effort discovery, not a correctness invariant.
//  2. Fetch the embedding bytes for that hash and decode to []float32.
//  3. Ask VectorStorage for the k+1 nearest neighbours (k+1 leaves room to
//     drop the source itself from its own result set).
//  4. Filter out the self-hit and hits below threshold.
//  5. Emit at most k candidates.
//
// Errors from VectorStorage.Search propagate wrapped. Errors from the
// EmbeddingLookup (DB-level failures distinct from "ready=false") also
// propagate. An empty sourceNodeIDs returns (nil, nil).
func (l *Linker) Candidates(ctx context.Context, repoID, branch string, sourceNodeIDs []string) ([]Candidate, error) {
	if len(sourceNodeIDs) == 0 {
		return nil, nil
	}

	out := make([]Candidate, 0, len(sourceNodeIDs)*l.k)
	for _, src := range sourceNodeIDs {
		hash, ready, err := l.refs.ContentHashForNode(ctx, repoID, branch, src)
		if err != nil {
			return nil, fmt.Errorf("autolink: lookup content hash for %s: %w", src, err)
		}
		if !ready || hash == "" {
			continue
		}
		blob, dim, found, err := l.refs.LookupExisting(ctx, hash)
		if err != nil {
			return nil, fmt.Errorf("autolink: lookup embedding for %s: %w", src, err)
		}
		if !found || dim == 0 || len(blob) < dim*4 {
			continue
		}
		vec := veccodec.DecodeFloat32LE(blob, dim)

		hits, err := l.vectors.Search(ctx, repoID, branch, vec, l.k+1, domain.VectorFilter{})
		if err != nil {
			return nil, fmt.Errorf("autolink: vector search for %s: %w", src, err)
		}

		emitted := 0
		for _, h := range hits {
			if emitted >= l.k {
				break
			}
			if h.NodeID == src {
				continue
			}
			if h.Score < l.threshold {
				continue
			}
			out = append(out, Candidate{
				SourceNodeID: src,
				TargetNodeID: h.NodeID,
				Score:        h.Score,
				RepoID:       repoID,
				Branch:       branch,
			})
			emitted++
		}
	}

	if l.metrics != nil && len(out) > 0 {
		l.metrics.AutolinkCandidates.WithLabelValues(repoID).Add(float64(len(out)))
	}
	return out, nil
}
