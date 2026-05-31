package application_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestPromote_EnqueuesPendingEmbedRefs verifies that Promote inserts one
// node_embedding_refs row per promoted node with state='pending',
// content_hash=NULL, embedded_at=NULL.
func TestPromote_EnqueuesPendingEmbedRefs(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n1, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	n2, _ := domain.NewNode(domain.NodeSpec{ID: "n2", Path: "b.go", Name: "B", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n1}, Edges: nil})
	sa.Stage("repo1", "main", "b.go", staging.File{Nodes: []*domain.Node{n2}, Edges: nil})

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var pending int
	err := db.QueryRow(`SELECT COUNT(*) FROM node_embedding_refs WHERE state='pending'`).Scan(&pending)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 2 {
		t.Errorf("pending refs: want 2, got %d", pending)
	}

	// Both rows must have NULL content_hash and NULL embedded_at, non-zero enqueued_at.
	rows, err := db.Query(`SELECT node_id, content_hash, embedded_at, enqueued_at FROM node_embedding_refs ORDER BY node_id`)
	if err != nil {
		t.Fatalf("select refs: %v", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var nodeID string
		var ch, ea sql.NullString
		var enq int64
		if err := rows.Scan(&nodeID, &ch, &ea, &enq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if ch.Valid {
			t.Errorf("node %s: content_hash want NULL, got %q", nodeID, ch.String)
		}
		if ea.Valid {
			t.Errorf("node %s: embedded_at want NULL, got %q", nodeID, ea.String)
		}
		if enq == 0 {
			t.Errorf("node %s: enqueued_at want non-zero", nodeID)
		}
		seen[nodeID] = true
	}
	if !seen["n1"] || !seen["n2"] {
		t.Errorf("missing nodes in refs table: %v", seen)
	}
}

// TestPromote_RepromoteResetsEmbedRef verifies that re-promoting a node whose
// ref is already 'ready' resets it back to 'pending' so the worker re-embeds.
func TestPromote_RepromoteResetsEmbedRef(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := staging.NewArea()
	n1, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n1}, Edges: nil})

	p := newTestPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-1",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 1: %v", err)
	}

	// Simulate the worker marking the ref ready.
	if _, err := db.Exec(`INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at) VALUES('h1','m',3,X'00',1)`); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
	if _, err := db.Exec(`UPDATE node_embedding_refs SET state='ready', content_hash='h1', embedded_at=1 WHERE node_id='n1'`); err != nil {
		t.Fatalf("seed ready: %v", err)
	}

	// Re-promote.
	n1b, _ := domain.NewNode(domain.NodeSpec{ID: "n1", Path: "a.go", Name: "A", Kind: domain.KindFunction})
	sa.Stage("repo1", "main", "a.go", staging.File{Nodes: []*domain.Node{n1b}, Edges: nil})
	if err := p.Promote(context.Background(), "repo1", "main", "sha-2",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 2: %v", err)
	}

	var state string
	var ch, ea sql.NullString
	if err := db.QueryRow(`SELECT state, content_hash, embedded_at FROM node_embedding_refs WHERE node_id='n1'`).Scan(&state, &ch, &ea); err != nil {
		t.Fatalf("requery: %v", err)
	}
	if state != "pending" {
		t.Errorf("state after re-promote: want pending, got %q", state)
	}
	if ch.Valid {
		t.Errorf("content_hash after re-promote: want NULL, got %q", ch.String)
	}
	if ea.Valid {
		t.Errorf("embedded_at after re-promote: want NULL, got %q", ea.String)
	}
}
