package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// EdgeRepo is the SQLite adapter for the ports.EdgeStorage port. It writes
// to the `edges` table created by migration 0001.
//
// SaveEdges uses ON CONFLICT(edge_id, branch) DO UPDATE SET score only: the
// first writer still wins for identity and confidence — re-running a flow
// (e.g. an auto-link queue handler) that proposes an already-existing edge
// never downgrades a definite or resolver-derived edge to Unresolved, because
// the conflict clause leaves confidence/last_promoted_at untouched. The single
// exception is `score`, which refreshes from the incoming row when that row
// carries one (COALESCE keeps the stored value when the writer passes NULL).
// This lets a `veska reindex` backfill/refresh similarity scores on SIMILAR_TO
// edges (solov2-c1s4) without disturbing any other edge attribute. edge_id
// derives from (src, kind, tgt), so a conflict is always same-kind — a
// non-SIMILAR_TO writer passing score=NULL is a guaranteed no-op here.
type EdgeRepo struct {
	db *sql.DB
}

// NewEdgeRepo constructs an EdgeRepo bound to the write-capable *sql.DB.
func NewEdgeRepo(db *sql.DB) *EdgeRepo {
	return &EdgeRepo{db: db}
}

// confidenceText maps the domain Confidence enum onto the TEXT column value
// stored in `edges.confidence`. The mapping is closed: an unknown int falls
// through to "unresolved" as a defensive default.
func confidenceText(c domain.Confidence) string {
	switch c {
	case domain.Definite:
		return "definite"
	case domain.Strong:
		return "strong"
	case domain.Probable:
		return "probable"
	default:
		return "unresolved"
	}
}

// SaveEdges persists edges into the `edges` table in a single transaction.
// Re-saving the same edge is idempotent for identity and confidence: the
// ON CONFLICT clause leaves the existing row in place and refreshes only
// `score` (see the type doc). An empty edges slice short-circuits without a
// round-trip.
func (r *EdgeRepo) SaveEdges(ctx context.Context, repoID, branch string, edges []*domain.Edge) error {
	if len(edges) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const stmt = `
INSERT INTO edges (
    edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, score, last_promoted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(edge_id, branch) DO UPDATE SET score = COALESCE(excluded.score, edges.score)`

	ps, err := tx.PrepareContext(ctx, stmt)
	if err != nil {
		return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: prepare: %w", err)
	}
	defer ps.Close()

	now := time.Now().UnixMilli()
	for _, e := range edges {
		if e == nil {
			continue
		}
		var score any
		if e.Score != nil {
			score = *e.Score
		}
		if _, err := ps.ExecContext(ctx,
			e.ID, branch, repoID, string(e.Src), string(e.Tgt),
			string(e.Kind), confidenceText(e.Confidence), score, now,
		); err != nil {
			return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: insert %s: %w", e.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: commit: %w", err)
	}
	return nil
}
