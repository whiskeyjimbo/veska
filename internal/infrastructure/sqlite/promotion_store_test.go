package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func systemActor() domain.Actor {
	return domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}
}

// TestPromotionStore_UnregisteredRepo verifies the registration check returns
// application.ErrUnregisteredRepo (type-assertable) for an unknown repo.
func TestPromotionStore_UnregisteredRepo(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	store := sqlite.NewPromotionStore(db, sqlite.NewFTSSink(), sqlite.NewEmbedRefSink())

	n, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "ghost", Branch: "main", GitSHA: "sha", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n}}},
	})
	var unreg application.ErrUnregisteredRepo
	if !errors.As(err, &unreg) {
		t.Fatalf("want ErrUnregisteredRepo, got %T: %v", err, err)
	}
	if unreg.RepoID != "ghost" {
		t.Errorf("RepoID = %q, want ghost", unreg.RepoID)
	}
}

// TestPromotionStore_RollsBackOnMidTxFailure proves the transaction is atomic:
// when a co-transactional write fails mid-promotion, every node/queue/FTS write
// from that Promote call is rolled back, leaving the prior committed state
// untouched.
func TestPromotionStore_RollsBackOnMidTxFailure(t *testing.T) {
	t.Parallel()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		"repo1", "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, sqlite.NewFTSSink(), sqlite.NewEmbedRefSink())

	// First promotion commits cleanly: 1 node.
	n1, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n1}}},
	}); err != nil {
		t.Fatalf("first Promote: %v", err)
	}

	var nodes, queue int
	db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes)
	db.QueryRow(`SELECT COUNT(*) FROM post_promotion_queue`).Scan(&queue)
	if nodes != 1 {
		t.Fatalf("after first promote: nodes=%d want 1", nodes)
	}

	// Sabotage the FTS table so the next promotion fails mid-transaction,
	// AFTER the node rows for the file have been deleted+inserted.
	if _, err := db.Exec(`DROP TABLE node_fts_trigrams`); err != nil {
		t.Fatalf("drop fts table: %v", err)
	}

	// Second promotion: changes the node and adds a sibling. It must fail and
	// roll back completely.
	n1b, _ := domain.NewNode("n1", "a.go", "A-changed", domain.KindFunction)
	n2, _ := domain.NewNode("n2", "a.go", "B", domain.KindFunction)
	err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "sha-2", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{{Path: "a.go", Nodes: []*domain.Node{n1b, n2}}},
	})
	if err == nil {
		t.Fatal("expected mid-tx failure, got nil")
	}

	// The prior committed state must be intact: still exactly 1 node, the
	// original symbol, and the original queue rows — nothing from the failed
	// promotion leaked.
	var nodes2, queue2 int
	var symbol string
	db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodes2)
	db.QueryRow(`SELECT COUNT(*) FROM post_promotion_queue`).Scan(&queue2)
	if err := db.QueryRow(`SELECT symbol_path FROM nodes WHERE node_id='n1'`).Scan(&symbol); err != nil {
		t.Fatalf("requery n1: %v", err)
	}
	if nodes2 != 1 {
		t.Errorf("nodes after rolled-back promote: want 1, got %d", nodes2)
	}
	if symbol != "A" {
		t.Errorf("symbol after rollback: want original %q, got %q", "A", symbol)
	}
	if queue2 != queue {
		t.Errorf("queue rows after rollback: want %d (unchanged), got %d", queue, queue2)
	}
}
