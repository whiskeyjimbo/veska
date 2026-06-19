// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// AuditEntry is an immutable record of a single state-changing operation.
// All fields are plain values so the entry can be serialized without reflection.
type AuditEntry struct {
	RepoID string

	// ActorID must follow domain.Actor ID conventions.
	ActorID string

	ActorKind domain.ActorKind

	// Op is a short, dot-separated verb that names the operation
	// (e.g., "node.save", "file.delete", "branch.seal").
	Op string

	TargetID string

	Branch string

	CreatedAt time.Time

	// Reason is the optional justification for the operation. Empty when none
	// was given; it is omitted from the serialized record when empty.
	Reason string
}

// AuditWriter appends audit entries to a durable log.
type AuditWriter interface {
	// Write must be safe for concurrent use and must not mutate e.
	Write(ctx context.Context, e AuditEntry) error
}
