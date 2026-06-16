package doctor

import (
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// Envelope is the common JSON wrapper emitted by all "doctor --json" subcommands,
// matching the contract.
type Envelope struct {
	SchemaVersion int           `json:"schema_version"`
	Subsystem     string        `json:"subsystem"`
	Status        health.Status `json:"status"`
	TS            string        `json:"ts"` // RFC3339
	Data          any           `json:"data"`
}

// NewEnvelope constructs an Envelope stamped with the current UTC time.
func NewEnvelope(subsystem string, status health.Status, data any) Envelope {
	return Envelope{
		SchemaVersion: 1,
		Subsystem:     subsystem,
		Status:        status,
		TS:            time.Now().UTC().Format(time.RFC3339),
		Data:          data,
	}
}
