package domain_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func TestEmbeddingRowFields(t *testing.T) {
	var r domain.EmbeddingRow
	if r.NodeID != "" {
		t.Errorf("NodeID zero value: want empty string, got %q", r.NodeID)
	}
	if r.ContentHash != "" {
		t.Errorf("ContentHash zero value: want empty string, got %q", r.ContentHash)
	}
	if r.ModelID != "" {
		t.Errorf("ModelID zero value: want empty string, got %q", r.ModelID)
	}
	if r.Vector != nil {
		t.Errorf("Vector zero value: want nil, got %v", r.Vector)
	}

	r2 := domain.EmbeddingRow{
		NodeID:      "node-1",
		ContentHash: "abc123",
		ModelID:     "nomic-embed-text",
		Vector:      []float32{0.1, 0.2, 0.3},
	}
	if r2.NodeID != "node-1" {
		t.Errorf("NodeID: want node-1, got %q", r2.NodeID)
	}
	if r2.ContentHash != "abc123" {
		t.Errorf("ContentHash: want abc123, got %q", r2.ContentHash)
	}
	if r2.ModelID != "nomic-embed-text" {
		t.Errorf("ModelID: want nomic-embed-text, got %q", r2.ModelID)
	}
	if len(r2.Vector) != 3 {
		t.Errorf("Vector len: want 3, got %d", len(r2.Vector))
	}
}

func TestHitFields(t *testing.T) {
	var h domain.SearchHit
	if h.NodeID != "" {
		t.Errorf("NodeID zero value: want empty string, got %q", h.NodeID)
	}
	if h.Score != 0 {
		t.Errorf("Score zero value: want 0, got %f", h.Score)
	}

	h2 := domain.SearchHit{NodeID: "node-42", Score: 0.95}
	if h2.NodeID != "node-42" {
		t.Errorf("NodeID: want node-42, got %q", h2.NodeID)
	}
	if h2.Score != 0.95 {
		t.Errorf("Score: want 0.95, got %f", h2.Score)
	}
}

func TestFilterFields(t *testing.T) {
	var f domain.VectorFilter
	if f.RepoID != "" {
		t.Errorf("RepoID zero value: want empty string, got %q", f.RepoID)
	}
	if f.Branch != "" {
		t.Errorf("Branch zero value: want empty string, got %q", f.Branch)
	}
	if f.ModelID != "" {
		t.Errorf("ModelID zero value: want empty string, got %q", f.ModelID)
	}

	f2 := domain.VectorFilter{RepoID: "repo-1", Branch: "main", ModelID: "nomic-embed-text"}
	if f2.RepoID != "repo-1" {
		t.Errorf("RepoID: want repo-1, got %q", f2.RepoID)
	}
	if f2.Branch != "main" {
		t.Errorf("Branch: want main, got %q", f2.Branch)
	}
	if f2.ModelID != "nomic-embed-text" {
		t.Errorf("ModelID: want nomic-embed-text, got %q", f2.ModelID)
	}
}
