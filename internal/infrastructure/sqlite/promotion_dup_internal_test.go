// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// countingSink records how many times AfterNodeInsert fires so the test can
// assert sinks run exactly once per kept node - never for a dropped duplicate.
type countingSink struct {
	afterInserts []string
}

func (s *countingSink) Prepare(context.Context, *sql.Tx) error { return nil }
func (s *countingSink) BeforeNodeDelete(context.Context, *sql.Tx, string, string, string) error {
	return nil
}

func (s *countingSink) AfterNodeInsert(_ context.Context, _ *sql.Tx, n nodeWrite, _ int64) error {
	s.afterInserts = append(s.afterInserts, n.NodeID)
	return nil
}

// TestPromotionStore_DuplicateNodeIDDoesNotAbortRepo guards against a
// single node_id collision within one batch (two distinct symbols hashing to
// the same id) must be dropped-with-warning, not abort the whole atomic
// promotion and leave the repo at zero nodes.
func TestPromotionStore_DuplicateNodeIDDoesNotAbortRepo(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "v.db")
	db, err := OpenWithOptions(dbPath, Options{BackupDir: filepath.Join(t.TempDir(), "backups")})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	sink := &countingSink{}
	store := NewPromotionStore(db, []PromotionSink{sink})

	// Two distinct symbols colliding on node_id "dup" (e.g. two `func _()` in
	// one file, which hash to the same (repo, path, kind, name)), plus one
	// healthy node that must survive.
	first := mustDupNode(t, "dup", "a.go", "First", domain.KindFunction)
	second := mustDupNode(t, "dup", "a.go", "Second", domain.KindFunction)
	healthy := mustDupNode(t, "ok", "a.go", "Healthy", domain.KindFunction)

	actor := domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: actor,
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			{Path: "a.go", Nodes: []*domain.Node{first, second, healthy}},
		},
	}); err != nil {
		t.Fatalf("Promote with duplicate node_id should succeed, got: %v", err)
	}

	// Repo is indexed, not zeroed: both distinct ids are present (the duplicate
	// collapsed to one row, the healthy node survived).
	var nodes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 2 {
		t.Errorf("nodes after dup-collision promote: want 2, got %d", nodes)
	}

	// The kept row is the first occurrence; the second was dropped.
	var keptName string
	if err := db.QueryRow(`SELECT symbol_path FROM nodes WHERE node_id='dup'`).Scan(&keptName); err != nil {
		t.Fatalf("query kept dup row: %v", err)
	}
	if keptName != "First" {
		t.Errorf("kept dup symbol: want First, got %q", keptName)
	}

	// Sinks fired exactly once per persisted node - never for the dropped dup.
	wantAfter := []string{"dup", "ok"}
	if len(sink.afterInserts) != len(wantAfter) {
		t.Fatalf("AfterNodeInsert calls: want %v, got %v", wantAfter, sink.afterInserts)
	}
	got := map[string]int{}
	for _, id := range sink.afterInserts {
		got[id]++
	}
	if got["dup"] != 1 || got["ok"] != 1 {
		t.Errorf("AfterNodeInsert per node: want dup=1 ok=1, got %v", got)
	}
}

func mustDupNode(t *testing.T, id, path, name string, kind domain.NodeKind) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(domain.NodeSpec{ID: id, Path: path, Name: name, Kind: kind})
	if err != nil {
		t.Fatalf("NewNode(%s): %v", id, err)
	}
	return n
}
