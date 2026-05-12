package domain

import (
	"testing"
	"time"
)

// ── SuppressionScope tests ─────────────────────────────────────────────────

func TestSuppressionScope_ValidValues(t *testing.T) {
	scopes := []SuppressionScope{
		ScopeFinding,
		ScopeSymbol,
		ScopeFile,
		ScopeRepo,
	}
	for _, s := range scopes {
		if s == "" {
			t.Errorf("SuppressionScope constant must not be empty")
		}
	}
}

// ── Suppression constructor tests ─────────────────────────────────────────

func TestNewSuppression_BranchAgnostic(t *testing.T) {
	now := time.Now()
	s, err := NewSuppression("sup-1", ScopeSymbol, "node-xyz",
		"noise", "actor-1", ActorKindHuman, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ID != "sup-1" {
		t.Errorf("id mismatch")
	}
	if s.Scope != ScopeSymbol {
		t.Errorf("scope mismatch")
	}
	if s.Target != "node-xyz" {
		t.Errorf("target mismatch")
	}
	if s.Reason != "noise" {
		t.Errorf("reason mismatch")
	}
	if s.ActorID != "actor-1" {
		t.Errorf("actor_id mismatch")
	}
	if s.ActorKind != ActorKindHuman {
		t.Errorf("actor_kind mismatch")
	}
	if s.Branch != nil {
		t.Error("branch should be nil for branch-agnostic suppression")
	}
	if s.ExpiresAt != nil {
		t.Error("expires_at should be nil by default")
	}
}

func TestNewSuppression_WithBranch(t *testing.T) {
	now := time.Now()
	s, err := NewSuppression("sup-2", ScopeFile, "pkg/foo.go",
		"temp", "actor-2", ActorKindAgent, now,
		WithBranch("feature-x"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Branch == nil || *s.Branch != "feature-x" {
		t.Error("branch not set correctly")
	}
}

func TestNewSuppression_WithExpiresAt(t *testing.T) {
	now := time.Now()
	exp := now.Add(24 * time.Hour)
	s, err := NewSuppression("sup-3", ScopeRepo, "repo-1",
		"temporary", "actor-3", ActorKindSystem, now,
		WithExpiresAt(exp),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.Equal(exp) {
		t.Error("expires_at not set correctly")
	}
}

func TestNewSuppression_ExpiresAt_MustBeAfterCreatedAt(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	_, err := NewSuppression("sup-4", ScopeRepo, "repo-1",
		"temp", "actor", ActorKindHuman, now,
		WithExpiresAt(past),
	)
	if err == nil {
		t.Error("expected error: expires_at must be after created_at")
	}
}

func TestNewSuppression_FindingScope_RequiresNonEmptyTarget(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression("sup-5", ScopeFinding, "",
		"reason", "actor", ActorKindHuman, now)
	if err == nil {
		t.Error("expected error: finding scope requires non-empty target (finding_id)")
	}
}

func TestNewSuppression_WithRule(t *testing.T) {
	now := time.Now()
	s, err := NewSuppression("sup-6", ScopeSymbol, "node-1",
		"noisy rule", "actor-1", ActorKindHuman, now,
		WithSuppressionRule("no-unused-exports"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Rule == nil || *s.Rule != "no-unused-exports" {
		t.Error("rule not set correctly")
	}
}

func TestNewSuppression_ErrorEmptyID(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression("", ScopeSymbol, "node-1",
		"reason", "actor", ActorKindHuman, now)
	if err == nil {
		t.Error("expected error for empty id")
	}
}

func TestNewSuppression_ErrorEmptyTarget(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression("sup-1", ScopeSymbol, "",
		"reason", "actor", ActorKindHuman, now)
	if err == nil {
		t.Error("expected error for empty target")
	}
}

func TestNewSuppression_ErrorEmptyReason(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression("sup-1", ScopeSymbol, "node-1",
		"", "actor", ActorKindHuman, now)
	if err == nil {
		t.Error("expected error for empty reason")
	}
}

func TestNewSuppression_ErrorEmptyActorID(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression("sup-1", ScopeSymbol, "node-1",
		"reason", "", ActorKindHuman, now)
	if err == nil {
		t.Error("expected error for empty actor_id")
	}
}

// ── ActorKind tests ────────────────────────────────────────────────────────

func TestActorKind_ValidValues(t *testing.T) {
	kinds := []ActorKind{ActorKindHuman, ActorKindAgent, ActorKindSystem}
	for _, k := range kinds {
		if k == "" {
			t.Errorf("ActorKind constant must not be empty")
		}
	}
}
