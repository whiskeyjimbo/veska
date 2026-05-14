package doctor

import "time"

// Envelope is the common JSON wrapper emitted by all "doctor --json" subcommands,
// matching the SOLO-13 §2.1 contract.
type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Subsystem     string `json:"subsystem"`
	Status        string `json:"status"`
	Ts            string `json:"ts"` // RFC3339
	Data          any    `json:"data"`
}

// NewEnvelope constructs an Envelope stamped with the current UTC time.
func NewEnvelope(subsystem, status string, data any) Envelope {
	return Envelope{
		SchemaVersion: 1,
		Subsystem:     subsystem,
		Status:        status,
		Ts:            time.Now().UTC().Format(time.RFC3339),
		Data:          data,
	}
}
