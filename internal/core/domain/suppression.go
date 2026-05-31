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

// SuppressionSpec carries the required fields of a Suppression. It groups the
// constructor's positional arguments into a named struct so the adjacent
// same-typed fields (Target/Reason/ActorID) cannot be transposed at a call
// site. Optional fields (rule, expiry, branch) are still supplied via
// SuppressionOption.
type SuppressionSpec struct {
	ID        string
	Scope     SuppressionScope
	Target    string
	Reason    string
	ActorID   string
	ActorKind ActorKind
	CreatedAt time.Time
}

// NewSuppression constructs a validated Suppression from spec.
//
// Invariants enforced:
//  1. ID, Target, Reason, ActorID must be non-empty.
//  2. Scope must be a valid enum value.
//  3. ActorKind must be a recognised ActorKind (same check NewActor enforces).
//  4. When Scope == ScopeFinding, Target must be a non-empty finding_id.
//  5. expires_at, when set, must be after created_at (enforced by WithExpiresAt).
func NewSuppression(spec SuppressionSpec, opts ...SuppressionOption) (*Suppression, error) {
	if spec.ID == "" {
		return nil, errors.New("suppression: id must not be empty")
	}
	if spec.Target == "" {
		return nil, errors.New("suppression: target must not be empty")
	}
	if spec.Reason == "" {
		return nil, errors.New("suppression: reason must not be empty")
	}
	if spec.ActorID == "" {
		return nil, errors.New("suppression: actor_id must not be empty")
	}
	if _, ok := validSuppressionScopes[spec.Scope]; !ok {
		return nil, errors.New("suppression: invalid scope")
	}
	if _, ok := validActorKinds[spec.ActorKind]; !ok {
		return nil, errors.New("suppression: invalid actor_kind")
	}
	if spec.Scope == ScopeFinding && spec.Target == "" {
		return nil, errors.New("suppression: scope=finding requires a non-empty finding_id target")
	}

	s := &Suppression{
		ID:        spec.ID,
		Scope:     spec.Scope,
		Target:    spec.Target,
		Reason:    spec.Reason,
		ActorID:   spec.ActorID,
		ActorKind: spec.ActorKind,
		CreatedAt: spec.CreatedAt,
	}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}
