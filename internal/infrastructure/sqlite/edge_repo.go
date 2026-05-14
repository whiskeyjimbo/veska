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
// SaveEdges uses ON CONFLICT(edge_id, branch) DO NOTHING so the first writer
// wins: re-running a flow (e.g. an auto-link queue handler) that proposes an
// already-existing edge is a safe no-op, and we never downgrade a definite or
// resolver-derived edge to Unresolved.
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
// Re-saving the same edge is idempotent: ON CONFLICT DO NOTHING leaves the
// existing row untouched. An empty edges slice short-circuits without a
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
    edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(edge_id, branch) DO NOTHING`

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
		if _, err := ps.ExecContext(ctx,
			e.ID, branch, repoID, string(e.Src), string(e.Tgt),
			string(e.Kind), confidenceText(e.Confidence), now,
		); err != nil {
			return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: insert %s: %w", e.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite.EdgeRepo.SaveEdges: commit: %w", err)
	}
	return nil
}
