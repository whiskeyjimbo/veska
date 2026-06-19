// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// PipelinesReport summarizes the review pipeline's token-budget state for the
// doctor subcommand.
//
//	Status "healthy" - tokens_today is below the configured daily cap (or
//	  no daily cap is configured).
//	Status "degraded" - tokens_today has reached the daily cap; the review
//	  pipeline is paused until the local-midnight window reset.
//	Status "broken" - the probe could not read the persisted token total.
//
// MaxTokensPerDay / MaxTokensPerCommit of 0 mean "unlimited" (cap disabled).
type PipelinesReport struct {
	TokensToday        int           `json:"tokens_today"`
	MaxTokensPerDay    int           `json:"max_tokens_per_day"`
	MaxTokensPerCommit int           `json:"max_tokens_per_commit"`
	Paused             bool          `json:"paused"`
	Status             health.Status `json:"status"`
}

// tokensTodayReader is the minimal surface CheckPipelines needs. Defined here
// (rather than imported from review/sqlite) so the doctor package stays
// unidirectional: production callers pass a *review.Quota, which satisfies
// this shape.
type tokensTodayReader interface {
	TokensToday(ctx context.Context) (int, error)
}

// CheckPipelines reports the review pipeline's token usage against the
// configured caps. A nil reader or a read failure yields Status "broken" with
// a nil error so callers can safely render the report.
func CheckPipelines(ctx context.Context, reader tokensTodayReader, maxPerDay, maxPerCommit int) (PipelinesReport, error) {
	if reader == nil {
		return PipelinesReport{Status: health.StatusBroken}, nil
	}
	tokens, err := reader.TokensToday(ctx)
	if err != nil {
		return PipelinesReport{Status: health.StatusBroken}, nil
	}

	report := PipelinesReport{
		TokensToday:        tokens,
		MaxTokensPerDay:    maxPerDay,
		MaxTokensPerCommit: maxPerCommit,
		Status:             health.StatusHealthy,
	}
	if maxPerDay > 0 && tokens >= maxPerDay {
		report.Paused = true
		report.Status = health.StatusDegraded
	}
	return report, nil
}
