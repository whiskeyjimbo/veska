// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"errors"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

var validActorKinds = map[domain.ActorKind]struct{}{
	domain.ActorKindHuman:  {},
	domain.ActorKindAgent:  {},
	domain.ActorKindSystem: {},
}

// ValidateAuditEntry ensures the audit log entry is structurally complete before writing.
func ValidateAuditEntry(e ports.AuditEntry) error {
	if e.RepoID == "" {
		return errors.New("audit: RepoID must not be empty")
	}
	if e.ActorID == "" {
		return errors.New("audit: ActorID must not be empty")
	}
	if _, ok := validActorKinds[e.ActorKind]; !ok {
		return errors.New("audit: ActorKind is missing or not a recognized value")
	}
	if e.Op == "" {
		return errors.New("audit: Op must not be empty")
	}
	return nil
}
