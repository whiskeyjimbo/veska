// Package health defines the typed health Status vocabulary shared by the
// doctor probes and the embedder probe. It is a zero-internal-dependency leaf:
// it imports nothing from the rest of veska so that lightweight callers (e.g.
// the embedderprobe used by `veska init`) do not transitively pull in the
// heavy doctor dependency tree (application/review, infrastructure/sqlite,
// infrastructure/vector).
// A Status has underlying type string and marshals to its lowercase word, so
// retyping a `json:"status"` struct field from string to Status keeps the JSON
// wire format byte-identical.
package health

// Status is a typed health state. Its three values form a total order from
// best to worst: healthy < degraded < broken.
type Status string

const (
	// StatusHealthy means all checks passed.
	StatusHealthy Status = "healthy"
	// StatusDegraded means the subsystem is functional but in a warning state.
	StatusDegraded Status = "degraded"
	// StatusBroken means the subsystem is faulted.
	StatusBroken Status = "broken"
)

// rank returns the precedence of a Status (higher == worse). Values outside
// the known set rank lowest so they never escalate a rollup.
func (s Status) rank() int {
	switch s {
	case StatusBroken:
		return 2
	case StatusDegraded:
		return 1
	case StatusHealthy:
		return 0
	default:
		return -1
	}
}

// WorseThan reports whether s is a worse health state than o, using the
// precedence healthy < degraded < broken. Equal states are not worse than
// each other. It centralizes the rollup escalation previously hand-rolled in
// the doctor probes.
func (s Status) WorseThan(o Status) bool {
	return s.rank() > o.rank()
}
