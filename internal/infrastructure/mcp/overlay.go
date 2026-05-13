package mcp

// Envelope is the standard MCP response wrapper for all tool responses.
// Tools embed or compose this struct in their specific response types.
type Envelope struct {
	IncludedStaging bool     `json:"included_staging,omitempty"`
	DegradedReasons []string `json:"degraded_reasons,omitempty"`
}

// DaemonState provides the current degradation state to the overlay helper.
// Implementations are provided by the daemon bootstrap.
type DaemonState interface {
	// IsSyncing returns true if startup resync is in progress.
	IsSyncing() bool
	// IsReconciling returns true if wake-reconcile is in progress.
	IsReconciling() bool
}

// BuildEnvelope constructs the Envelope for a tool response.
// stagingRead: whether this tool attempted to read staging.
// stagingOK: whether the staging read succeeded (false triggers staging_unavailable).
// state: current daemon state (may be nil — treated as all-false).
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

	var degraded []string
	if len(reasons) > 0 {
		degraded = reasons
	}

	return Envelope{
		IncludedStaging: includedStaging,
		DegradedReasons: degraded,
	}
}

// AppendDegradedReason returns a new slice with reason appended (avoids mutating the original).
func AppendDegradedReason(reasons []string, reason string) []string {
	result := make([]string, len(reasons), len(reasons)+1)
	copy(result, reasons)
	return append(result, reason)
}
