package domain

import (
	"errors"
	"time"
)

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

// Suppression silences one or more findings that match its scope and target.
// When Branch is nil, the suppression applies globally on every branch;
// otherwise, it is branch-specific.
type Suppression struct {
	ID        string
	Scope     SuppressionScope
	Target    string
	Reason    string
	ActorID   string
	ActorKind ActorKind
	CreatedAt time.Time

	Rule      *string
	ExpiresAt *time.Time
	Branch    *string
}

type SuppressionOption func(*Suppression) error

// WithSuppressionRule restricts the suppression to a specific rule ID.
func WithSuppressionRule(rule string) SuppressionOption {
	return func(s *Suppression) error {
		s.Rule = &rule
		return nil
	}
}

// WithExpiresAt sets the suppression expiry time, which must be after CreatedAt.
func WithExpiresAt(t time.Time) SuppressionOption {
	return func(s *Suppression) error {
		if !t.After(s.CreatedAt) {
			return errors.New("suppression: expires_at must be after created_at")
		}
		s.ExpiresAt = &t
		return nil
	}
}

// WithBranch restricts the suppression to a specific branch.
func WithBranch(branch string) SuppressionOption {
	return func(s *Suppression) error {
		s.Branch = &branch
		return nil
	}
}

// SuppressionSpec groups the required fields of a Suppression into a struct to
// prevent transposing adjacent same-typed parameters at construction call sites.
type SuppressionSpec struct {
	ID        string
	Scope     SuppressionScope
	Target    string
	Reason    string
	ActorID   string
	ActorKind ActorKind
	CreatedAt time.Time
}

// NewSuppression constructs a validated Suppression from the specification,
// verifying that required fields are non-empty and that enum values are valid.
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
