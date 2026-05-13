package domain

import (
	"testing"
)

func TestNewActor_EmptyID(t *testing.T) {
	_, err := NewActor("", ActorKindHuman)
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
}

func TestNewActor_InvalidKind(t *testing.T) {
	_, err := NewActor("human:alice", ActorKind("wizard"))
	if err == nil {
		t.Fatal("expected error for invalid ActorKind, got nil")
	}
}

func TestNewActor_HappyPath_Human(t *testing.T) {
	a, err := NewActor("human:alice", ActorKindHuman)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ID != "human:alice" {
		t.Errorf("ID: got %q, want %q", a.ID, "human:alice")
	}
	if a.Kind != ActorKindHuman {
		t.Errorf("Kind: got %q, want %q", a.Kind, ActorKindHuman)
	}
}

func TestNewActor_HappyPath_Agent(t *testing.T) {
	a, err := NewActor("agent:claude", ActorKindAgent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Kind != ActorKindAgent {
		t.Errorf("Kind: got %q, want %q", a.Kind, ActorKindAgent)
	}
}

func TestNewActor_HappyPath_System(t *testing.T) {
	a, err := NewActor("service:engram", ActorKindSystem)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Kind != ActorKindSystem {
		t.Errorf("Kind: got %q, want %q", a.Kind, ActorKindSystem)
	}
}
