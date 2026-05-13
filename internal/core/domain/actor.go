package domain

import "errors"

// Actor is an attribution stamp that records who or what performed an action.
// It is a lightweight value type; use ActorKind (defined in finding.go) for
// the kind discriminator.
//
// Convention for the ID field:
//
//	"human:<username>"     — a human developer
//	"agent:<name>"         — an AI agent
//	"service:engram"       — the engram system itself
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
