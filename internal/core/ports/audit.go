package ports

import (
	"context"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// AuditEntry is an immutable record of a single state-changing operation.
// All fields are plain values so the entry can be serialised without reflection.
type AuditEntry struct {
	// RepoID identifies the repository in which the operation occurred.
	RepoID string

	// ActorID is the stable identifier of the actor (human, agent, or service)
	// that performed the operation. Format follows Actor.ID conventions.
	ActorID string

	// ActorKind classifies the actor (human / agent / service).
	ActorKind domain.ActorKind

	// Op is a short, dot-separated verb that names the operation
	// (e.g. "node.save", "file.delete", "branch.seal").
	Op string

	// TargetID is the identifier of the resource affected by the operation.
	TargetID string

	// Branch is the Dolt/Git branch on which the operation was performed.
	Branch string

	// CreatedAt is the wall-clock time at which the operation was recorded.
	CreatedAt time.Time

	// Reason is the optional human/agent-supplied justification for the
	// operation (e.g. why a finding was closed or reopened). Empty when none
	// was given; omitted from the serialised record when empty.
	Reason string
}

// AuditWriter is the port for appending audit entries to a durable log.
// Implementations are provided by infrastructure adapters (e.g. Dolt audit table,
// append-only file).
type AuditWriter interface {
	// Write appends e to the audit log. Implementations must be safe for
	// concurrent use. Write must not mutate e.
	Write(ctx context.Context, e AuditEntry) error
}
