// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

// Envelope wraps all tool responses to carry staging inclusion status and degradation reasons.
// DegradedReasons is intentionally not marked omitempty so that it serializes as an empty JSON array rather than null.
type Envelope struct {
	IncludedStaging bool     `json:"included_staging,omitempty"`
	DegradedReasons []string `json:"degraded_reasons"`
}

// DaemonState provides the current degradation state during startup resync or wake-reconciliation.
type DaemonState interface {
	IsSyncing() bool
	IsReconciling() bool
}

// BuildEnvelope constructs the Envelope for a tool response, capturing daemon degradation states and staging read statuses.
func BuildEnvelope(stagingRead bool, stagingOK bool, state DaemonState) Envelope {
	reasons := []string{}

	if state != nil && state.IsSyncing() {
		reasons = AppendDegradedReason(reasons, "startup_resync")
	}
	if state != nil && state.IsReconciling() {
		reasons = AppendDegradedReason(reasons, "wake_reconciling")
	}

	var includedStaging bool
	if stagingRead && !stagingOK {
		includedStaging = false
		reasons = AppendDegradedReason(reasons, "staging_unavailable")
	} else if stagingRead && stagingOK {
		includedStaging = true
	}

	// We return an empty slice instead of nil to guarantee the field serializes as an empty JSON array.
	return Envelope{
		IncludedStaging: includedStaging,
		DegradedReasons: reasons,
	}
}

// AppendDegradedReason returns a new slice with the reason appended, avoiding in-place mutation of the original slice.
func AppendDegradedReason(reasons []string, reason string) []string {
	result := make([]string, len(reasons), len(reasons)+1)
	copy(result, reasons)
	return append(result, reason)
}
