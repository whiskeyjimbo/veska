package doctor_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// TestNewEnvelopeJSONWireFormat pins the envelope wire format: retyping
// Status from string to health.Status must not change the lowercase status
// word emitted, and the schema_version/subsystem/status/data keys must remain.
func TestNewEnvelopeJSONWireFormat(t *testing.T) {
	env := doctor.NewEnvelope("storage", health.StatusHealthy, map[string]any{"k": "v"})
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"schema_version":1`,
		`"subsystem":"storage"`,
		`"status":"healthy"`,
		`"data":{"k":"v"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope JSON %s missing %s", got, want)
		}
	}
}
