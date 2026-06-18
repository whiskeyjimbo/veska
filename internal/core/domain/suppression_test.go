// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
	s, err := NewSuppression(SuppressionSpec{ID: "sup-1", Scope: ScopeSymbol, Target: "node-xyz", Reason: "noise", ActorID: "actor-1", ActorKind: ActorKindHuman, CreatedAt: now})
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
	s, err := NewSuppression(SuppressionSpec{ID: "sup-2", Scope: ScopeFile, Target: "pkg/foo.go", Reason: "temp", ActorID: "actor-2", ActorKind: ActorKindAgent, CreatedAt: now}, WithBranch("feature-x"))
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
	s, err := NewSuppression(SuppressionSpec{ID: "sup-3", Scope: ScopeRepo, Target: "repo-1", Reason: "temporary", ActorID: "actor-3", ActorKind: ActorKindSystem, CreatedAt: now}, WithExpiresAt(exp))
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
	_, err := NewSuppression(SuppressionSpec{ID: "sup-4", Scope: ScopeRepo, Target: "repo-1", Reason: "temp", ActorID: "actor", ActorKind: ActorKindHuman, CreatedAt: now}, WithExpiresAt(past))
	if err == nil {
		t.Error("expected error: expires_at must be after created_at")
	}
}

func TestNewSuppression_FindingScope_RequiresNonEmptyTarget(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "sup-5", Scope: ScopeFinding, Target: "", Reason: "reason", ActorID: "actor", ActorKind: ActorKindHuman, CreatedAt: now})
	if err == nil {
		t.Error("expected error: finding scope requires non-empty target (finding_id)")
	}
}

func TestNewSuppression_WithRule(t *testing.T) {
	now := time.Now()
	s, err := NewSuppression(SuppressionSpec{ID: "sup-6", Scope: ScopeSymbol, Target: "node-1", Reason: "noisy rule", ActorID: "actor-1", ActorKind: ActorKindHuman, CreatedAt: now}, WithSuppressionRule("no-unused-exports"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Rule == nil || *s.Rule != "no-unused-exports" {
		t.Error("rule not set correctly")
	}
}

func TestNewSuppression_ErrorEmptyID(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "", Scope: ScopeSymbol, Target: "node-1", Reason: "reason", ActorID: "actor", ActorKind: ActorKindHuman, CreatedAt: now})
	if err == nil {
		t.Error("expected error for empty id")
	}
}

func TestNewSuppression_ErrorEmptyTarget(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "sup-1", Scope: ScopeSymbol, Target: "", Reason: "reason", ActorID: "actor", ActorKind: ActorKindHuman, CreatedAt: now})
	if err == nil {
		t.Error("expected error for empty target")
	}
}

func TestNewSuppression_ErrorEmptyReason(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "sup-1", Scope: ScopeSymbol, Target: "node-1", Reason: "", ActorID: "actor", ActorKind: ActorKindHuman, CreatedAt: now})
	if err == nil {
		t.Error("expected error for empty reason")
	}
}

func TestNewSuppression_ErrorEmptyActorID(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "sup-1", Scope: ScopeSymbol, Target: "node-1", Reason: "reason", ActorID: "", ActorKind: ActorKindHuman, CreatedAt: now})
	if err == nil {
		t.Error("expected error for empty actor_id")
	}
}

func TestNewSuppression_ErrorInvalidActorKind(t *testing.T) {
	now := time.Now()
	_, err := NewSuppression(SuppressionSpec{ID: "sup-1", Scope: ScopeSymbol, Target: "node-1", Reason: "reason", ActorID: "actor", ActorKind: ActorKind("robot"), CreatedAt: now})
	if err == nil {
		t.Error("expected error for invalid actor_kind")
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
