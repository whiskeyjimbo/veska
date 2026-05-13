package domain

import (
	"errors"
	"time"
)

// ── SuppressionScope ───────────────────────────────────────────────────────

// SuppressionScope is a closed enum of the entities a suppression can target.
type SuppressionScope string

const (
	ScopeFinding SuppressionScope = "finding"
	ScopeSymbol  SuppressionScope = "symbol"
	ScopeFile    SuppressionScope = "file"
	ScopeRepo    SuppressionScope = "repo"
)

var validSuppressionScopes = map[SuppressionScope]struct{}{
	ScopeFinding: {},
	ScopeSymbol:  {},
	ScopeFile:    {},
	ScopeRepo:    {},
}

// ── Suppression ────────────────────────────────────────────────────────────

// Suppression silences one or more findings that match its scope and target.
//
// Branch semantics:
//   - Branch == nil → branch-agnostic: suppression applies on every branch.
//   - Branch != nil → branch-specific: suppression applies only on that branch.
type Suppression struct {
	ID        string
	Scope     SuppressionScope
	Target    string
	Reason    string
	ActorID   string
	ActorKind ActorKind
	CreatedAt time.Time

	// Optional fields.
	Rule      *string
	ExpiresAt *time.Time
	Branch    *string
}

// SuppressionOption is a functional option for NewSuppression.
type SuppressionOption func(*Suppression) error

// WithSuppressionRule restricts the suppression to a specific rule.
func WithSuppressionRule(rule string) SuppressionOption {
	return func(s *Suppression) error {
		s.Rule = &rule
		return nil
	}
}

// WithExpiresAt sets an expiry time. Must be after CreatedAt.
func WithExpiresAt(t time.Time) SuppressionOption {
	return func(s *Suppression) error {
		if !t.After(s.CreatedAt) {
			return errors.New("suppression: expires_at must be after created_at")
		}
		s.ExpiresAt = &t
		return nil
	}
}

// WithBranch makes the suppression branch-specific.
func WithBranch(branch string) SuppressionOption {
	return func(s *Suppression) error {
		s.Branch = &branch
		return nil
	}
}

// NewSuppression constructs a validated Suppression.
//
// Invariants enforced:
//  1. id, target, reason, actorID must be non-empty.
//  2. scope must be a valid enum value.
//  3. When scope == ScopeFinding, target must be a non-empty finding_id.
//  4. expires_at, when set, must be after created_at (enforced by WithExpiresAt).
func NewSuppression(
	id string,
	scope SuppressionScope,
	target, reason, actorID string,
	actorKind ActorKind,
	createdAt time.Time,
	opts ...SuppressionOption,
) (*Suppression, error) {
	if id == "" {
		return nil, errors.New("suppression: id must not be empty")
	}
	if target == "" {
		return nil, errors.New("suppression: target must not be empty")
	}
	if reason == "" {
		return nil, errors.New("suppression: reason must not be empty")
	}
	if actorID == "" {
		return nil, errors.New("suppression: actor_id must not be empty")
	}
	if _, ok := validSuppressionScopes[scope]; !ok {
		return nil, errors.New("suppression: invalid scope")
	}
	if scope == ScopeFinding && target == "" {
		return nil, errors.New("suppression: scope=finding requires a non-empty finding_id target")
	}

	s := &Suppression{
		ID:        id,
		Scope:     scope,
		Target:    target,
		Reason:    reason,
		ActorID:   actorID,
		ActorKind: actorKind,
		CreatedAt: createdAt,
	}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}
