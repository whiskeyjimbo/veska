// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// FindingStorage is the port for persisting Findings. Save must be safe for
// concurrent use and idempotent on the (finding_id, branch) primary key.
type FindingStorage interface {
	// Save must not mutate the given Finding.
	Save(ctx context.Context, f *domain.Finding) error

	// CloseObsolete closes the open finding identified by (findingID, branch),
	// setting closed_reason='revalidated_obsolete'. It is a no-op when no open
	// finding matches.
	CloseObsolete(ctx context.Context, findingID, branch string) error

	// CloseSupersededByRule closes every open finding of the given rule in
	// (repoID, branch) whose finding ID is not in keep. This is the reconciliation
	// primitive for authoritative checks (e.g., vulnerable dependency scans)
	// to ensure prior findings disappear automatically once the underlying condition
	// is resolved, avoiding permanently open findings. The call is idempotent and
	// safe for concurrent use.
	CloseSupersededByRule(ctx context.Context, repoID, branch, rule string, keep []string) error

	// CloseSupersededAutoLinks closes open auto-link findings in (repoID, branch)
	// for source node IDs that have new candidates or have drifted, ensuring the
	// open findings count does not balloon across re-promotions. The call is
	// idempotent, and an empty sourceNodeIDs slice is a no-op.
	CloseSupersededAutoLinks(ctx context.Context, repoID, branch string, sourceNodeIDs []string) error
}
