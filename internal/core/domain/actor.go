package domain

import "errors"

// ActorKind distinguishes who or what performed an action. It is a cross-cutting
// domain value used by Actor, Finding, and Suppression (and surfaced as
// domain.Actor across the application and MCP layers).
type ActorKind string

const (
	ActorKindHuman  ActorKind = "human"
	ActorKindAgent  ActorKind = "agent"
	ActorKindSystem ActorKind = "system"
)

var validActorKinds = map[ActorKind]struct{}{
	ActorKindHuman:  {},
	ActorKindAgent:  {},
	ActorKindSystem: {},
}

// Actor is an attribution stamp that records who or what performed an action.
// It is a lightweight value type; ActorKind is the kind discriminator.
//
// Convention for the ID field:
//
//	"human:<username>"     — a human developer
//	"agent:<name>"         — an AI agent
//	"service:veska"       — the veska system itself
type Actor struct {
	ID   string
	Kind ActorKind
}

// NewActor constructs a validated Actor. Returns an error if id is empty or
// kind is not a recognised ActorKind value.
func NewActor(id string, kind ActorKind) (*Actor, error) {
	if id == "" {
		return nil, errors.New("actor: id must not be empty")
	}
	if _, ok := validActorKinds[kind]; !ok {
		return nil, errors.New("actor: invalid actor_kind")
	}
	return &Actor{ID: id, Kind: kind}, nil
}
