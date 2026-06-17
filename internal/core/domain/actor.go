// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import "errors"

// ActorKind distinguishes who or what performed an action.
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
// The ID field follows these format conventions:
//
//	"human:<username>" - a human developer
//	"agent:<name>" - an AI agent
//	"service:veska" - the veska system itself
type Actor struct {
	ID   string
	Kind ActorKind
}

// NewActor constructs a validated Actor, returning an error if id is empty or
// kind is unrecognized.
func NewActor(id string, kind ActorKind) (*Actor, error) {
	if id == "" {
		return nil, errors.New("actor: id must not be empty")
	}
	if _, ok := validActorKinds[kind]; !ok {
		return nil, errors.New("actor: invalid actor_kind")
	}
	return &Actor{ID: id, Kind: kind}, nil
}
