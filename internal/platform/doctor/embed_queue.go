// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// Thresholds for the embed-queue health probe. The "broken" status only
// fires on a true outage (Failed > 0) - accumulating failed rows means
// rows are giving up after the worker's retry budget is exhausted. The
// "degraded" threshold is a soft signal that the queue is not draining
// fast enough; chosen at 1000 to align with M3 capacity expectations.
const (
	embedQueueDegradedPending = 1000
)

// EmbedQueueReport summarises the state of node_embedding_refs for the
// doctor subcommand.
//
//	Status "healthy" - no failed rows, pending < degraded threshold.
//	Status "degraded" - pending count > embedQueueDegradedPending; the
//	  embedder is keeping up correctness-wise but not drain-wise.
//	Status "broken" - at least one row has been parked in 'failed'.
//
// "broken" takes precedence over "degraded" when both conditions hold.
type EmbedQueueReport struct {
	Pending int           `json:"pending"`
	Ready   int           `json:"ready"`
	Failed  int           `json:"failed"`
	Status  health.Status `json:"status"`
}

// embedRefsCounter is the minimal surface CheckEmbedQueueHealth needs.
// Defined here (rather than imported from sqlite) so the doctor package
// stays unidirectional: callers wire in any implementation. Production
// callers pass *sqlite.EmbeddingRefsRepo, which satisfies this shape.
type embedRefsCounter interface {
	CountByState(ctx context.Context) (map[string]int, error)
}

// CheckEmbedQueueHealth queries refs for state counts and classifies the
// queue. A nil counter or a query failure yields Status "broken" with
// zeroed counts so callers can safely render the report.
func CheckEmbedQueueHealth(ctx context.Context, refs embedRefsCounter) (EmbedQueueReport, error) {
	if refs == nil {
		return EmbedQueueReport{Status: health.StatusBroken}, nil
	}
	counts, err := refs.CountByState(ctx)
	if err != nil {
		return EmbedQueueReport{Status: health.StatusBroken}, nil
	}

	report := EmbedQueueReport{
		Pending: counts["pending"],
		Ready:   counts["ready"],
		Failed:  counts["failed"],
		Status:  health.StatusHealthy,
	}
	switch {
	case report.Failed > 0:
		report.Status = health.StatusBroken
	case report.Pending > embedQueueDegradedPending:
		report.Status = health.StatusDegraded
	}
	return report, nil
}
