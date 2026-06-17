// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// openEdgeRepoTestDB creates a temporary database populated with a repository and source/destination nodes for testing.
func openEdgeRepoTestDB(t *testing.T) (*sql.DB, *sqlite.EdgeRepo) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	for _, id := range []string{"n-src", "n-tgt", "n-tgt2"} {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "main", "r1", "go", "function", id, id+".go",
			1, 10, "h-"+id, now, "service:veska", "system"); err != nil {
			t.Fatalf("insert node %s: %v", id, err)
		}
	}
	return db, sqlite.NewEdgeRepo(db)
}

// TestEdgeRepo_SaveEdges_PersistsUnresolvedSimilarTo verifies that SaveEdges writes edges with the expected properties.
func TestEdgeRepo_SaveEdges_PersistsUnresolvedSimilarTo(t *testing.T) {
	t.Parallel()
	db, repo := openEdgeRepoTestDB(t)

	e1, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))
	e2, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt2", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))

	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{e1, e2}); err != nil {
		t.Fatalf("SaveEdges: %v", err)
	}

	rows, err := db.Query(`SELECT edge_id, kind, confidence, src_node_id, dst_node_id
		FROM edges WHERE repo_id='r1' AND branch='main' ORDER BY edge_id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	got := map[string][4]string{}
	for rows.Next() {
		var id, kind, conf, src, dst string
		if err := rows.Scan(&id, &kind, &conf, &src, &dst); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = [4]string{kind, conf, src, dst}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d (%v)", len(got), got)
	}
	for _, e := range []*domain.Edge{e1, e2} {
		v, ok := got[e.ID]
		if !ok {
			t.Fatalf("missing edge_id %s", e.ID)
		}
		if v[0] != "SIMILAR_TO" {
			t.Errorf("kind = %q, want SIMILAR_TO", v[0])
		}
		if v[1] != "unresolved" {
			t.Errorf("confidence = %q, want unresolved", v[1])
		}
		if v[2] != string(e.Src) || v[3] != string(e.Tgt) {
			t.Errorf("src/dst = %q/%q, want %q/%q", v[2], v[3], e.Src, e.Tgt)
		}
	}
}

// TestEdgeRepo_SaveEdges_EmptyIsNoop verifies that calling SaveEdges with an empty batch is a no-op.
func TestEdgeRepo_SaveEdges_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	_, repo := openEdgeRepoTestDB(t)
	if err := repo.SaveEdges(context.Background(), "r1", "main", nil); err != nil {
		t.Fatalf("SaveEdges(nil): %v", err)
	}
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{}); err != nil {
		t.Fatalf("SaveEdges([]): %v", err)
	}
}

// TestEdgeRepo_SaveEdges_Idempotent verifies that SaveEdges is idempotent for a given edge ID and branch.
func TestEdgeRepo_SaveEdges_Idempotent(t *testing.T) {
	t.Parallel()
	db, repo := openEdgeRepoTestDB(t)

	e, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{e}); err != nil {
		t.Fatalf("first SaveEdges: %v", err)
	}
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{e}); err != nil {
		t.Fatalf("second SaveEdges: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE edge_id=? AND branch='main'`, e.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row after re-save, got %d", n)
	}
}

// TestEdgeRepo_SaveEdges_DoesNotDowngradeResolved ensures that saving an unresolved duplicate of an already resolved definite edge does not downgrade its confidence.
func TestEdgeRepo_SaveEdges_DoesNotDowngradeResolved(t *testing.T) {
	t.Parallel()
	db, repo := openEdgeRepoTestDB(t)

	definite, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Definite))
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{definite}); err != nil {
		t.Fatalf("save definite: %v", err)
	}

	unresolved, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{unresolved}); err != nil {
		t.Fatalf("save unresolved: %v", err)
	}

	var conf string
	if err := db.QueryRow(`SELECT confidence FROM edges WHERE edge_id=?`, definite.ID).Scan(&conf); err != nil {
		t.Fatalf("query: %v", err)
	}
	if conf != "definite" {
		t.Errorf("expected confidence to remain definite, got %q", conf)
	}
}

// TestEdgeRepo_SaveEdges_PersistsAndRefreshesScore verifies that the score column is updated on conflict and preserved when the new score is nil.
func TestEdgeRepo_SaveEdges_PersistsAndRefreshesScore(t *testing.T) {
	t.Parallel()
	db, repo := openEdgeRepoTestDB(t)
	ctx := context.Background()

	scored, _ := domain.NewEdge(
		domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo},
		domain.WithConfidence(domain.Unresolved), domain.WithScore(0.5))
	if err := repo.SaveEdges(ctx, "r1", "main", []*domain.Edge{scored}); err != nil {
		t.Fatalf("save scored: %v", err)
	}
	if got := queryScore(t, db, scored.ID); !approxEqual(got, 0.5) {
		t.Fatalf("after first save score = %v, want 0.5", got)
	}

	// Re-save the same pair with a higher score -> refreshes.
	rescored, _ := domain.NewEdge(
		domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo},
		domain.WithConfidence(domain.Unresolved), domain.WithScore(0.9))
	if err := repo.SaveEdges(ctx, "r1", "main", []*domain.Edge{rescored}); err != nil {
		t.Fatalf("save rescored: %v", err)
	}
	if got := queryScore(t, db, scored.ID); !approxEqual(got, 0.9) {
		t.Fatalf("after re-save score = %v, want 0.9 (refresh)", got)
	}

	// Re-save with NO score -> COALESCE preserves the stored value.
	noScore, _ := domain.NewEdge(
		domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo},
		domain.WithConfidence(domain.Unresolved))
	if err := repo.SaveEdges(ctx, "r1", "main", []*domain.Edge{noScore}); err != nil {
		t.Fatalf("save no-score: %v", err)
	}
	if got := queryScore(t, db, scored.ID); !approxEqual(got, 0.9) {
		t.Fatalf("after no-score re-save score = %v, want 0.9 (preserved)", got)
	}
}

func queryScore(t *testing.T, db *sql.DB, edgeID string) float64 {
	t.Helper()
	var score sql.NullFloat64
	if err := db.QueryRow(`SELECT score FROM edges WHERE edge_id=?`, edgeID).Scan(&score); err != nil {
		t.Fatalf("query score: %v", err)
	}
	if !score.Valid {
		t.Fatalf("score is NULL, expected a value")
	}
	return score.Float64
}

// approxEqual compares float values with a small delta tolerance.
func approxEqual(got, want float64) bool {
	d := got - want
	return d < 1e-6 && d > -1e-6
}

// TestEdgeRepo_SaveEdges_RoundTripID verifies that the persisted edge_id matches the generated domain ID.
func TestEdgeRepo_SaveEdges_RoundTripID(t *testing.T) {
	t.Parallel()
	db, repo := openEdgeRepoTestDB(t)

	e, _ := domain.NewEdge(domain.EdgeSpec{Src: "n-src", Tgt: "n-tgt", Kind: domain.EdgeSimilarTo}, domain.WithConfidence(domain.Unresolved))
	if err := repo.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{e}); err != nil {
		t.Fatalf("SaveEdges: %v", err)
	}

	var id string
	if err := db.QueryRow(`SELECT edge_id FROM edges WHERE src_node_id='n-src' AND dst_node_id='n-tgt'`).Scan(&id); err != nil {
		t.Fatalf("query: %v", err)
	}
	if id != e.ID {
		t.Errorf("persisted edge_id %q != domain ID %q", id, e.ID)
	}
}
