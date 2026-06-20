// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// EdgeRepo implements ports.EdgeStorage using a SQLite database.
// Conflict resolution updates only the score column on duplicate edge IDs,
// leaving confidence and promotion timestamps unchanged.
type EdgeRepo struct {
	db *sql.DB
}

// NewEdgeRepo constructs an EdgeRepo bound to the given sql.DB.
func NewEdgeRepo(db *sql.DB) *EdgeRepo {
	return &EdgeRepo{db: db}
}

// confidenceText maps the domain Confidence enum onto the corresponding TEXT column representation.
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

// SaveEdges persists edges into the edges table within a single SQL transaction.
// An empty slice of edges is a no-op.
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
