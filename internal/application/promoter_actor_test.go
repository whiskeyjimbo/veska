package application

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestPromote_ActorStoredInNodes verifies that the actor_id and actor_kind
// passed to Promote are written into every inserted node row.
func TestPromote_ActorStoredInNodes(t *testing.T) {
	tests := []struct {
		name     string
		actor    domain.Actor
		wantID   string
		wantKind string
	}{
		{
			name:     "system actor",
			actor:    domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem},
			wantID:   "service:veska",
			wantKind: "system",
		},
		{
			name:     "human actor",
			actor:    domain.Actor{ID: "human:alice", Kind: domain.ActorKindHuman},
			wantID:   "human:alice",
			wantKind: "human",
		},
		{
			name:     "agent actor",
			actor:    domain.Actor{ID: "agent:my-agent", Kind: domain.ActorKindAgent},
			wantID:   "agent:my-agent",
			wantKind: "agent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openMemDB(t)
			insertTestRepo(t, db, "repo1")

			sa := NewStagingArea()
			n, _ := domain.NewNode("n1", "a.go", "A", domain.KindFunction)
			sa.StageFile("repo1", "main", "a.go", []*domain.Node{n}, nil)

			p := NewPromoter(sa, db)
			if err := p.Promote(context.Background(), "repo1", "main", "sha-abc", tc.actor); err != nil {
				t.Fatalf("Promote: %v", err)
			}

			var gotID, gotKind string
			err := db.QueryRow(`SELECT actor_id, actor_kind FROM nodes WHERE node_id = 'n1'`).
				Scan(&gotID, &gotKind)
			if err != nil {
				t.Fatalf("query nodes: %v", err)
			}
			if gotID != tc.wantID {
				t.Errorf("actor_id = %q, want %q", gotID, tc.wantID)
			}
			if gotKind != tc.wantKind {
				t.Errorf("actor_kind = %q, want %q", gotKind, tc.wantKind)
			}
		})
	}
}
