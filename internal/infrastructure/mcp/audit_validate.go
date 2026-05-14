package mcp

import (
	"errors"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// validActorKinds is the closed set of recognised actor kinds.
var validActorKinds = map[domain.ActorKind]struct{}{
	domain.ActorKindHuman:  {},
	domain.ActorKindAgent:  {},
	domain.ActorKindSystem: {},
}

// ValidateAuditEntry returns an error if e is missing any required field or
// carries an unrecognised ActorKind. Call this before passing an AuditEntry to
// ports.AuditWriter.Write to guarantee the audit log is never incomplete.
//
// Required fields: RepoID, ActorID (non-empty), ActorKind (valid enum value), Op.
func ValidateAuditEntry(e ports.AuditEntry) error {
	if e.RepoID == "" {
		return errors.New("audit: RepoID must not be empty")
	}
	if e.ActorID == "" {
		return errors.New("audit: ActorID must not be empty")
	}
	if _, ok := validActorKinds[e.ActorKind]; !ok {
		return errors.New("audit: ActorKind is missing or not a recognised value")
	}
	if e.Op == "" {
		return errors.New("audit: Op must not be empty")
	}
	return nil
}
