// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package health_test

import (
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

func TestWorseThanOrdering(t *testing.T) {
	cases := []struct {
		name string
		s, o health.Status
		want bool
	}{
		{"broken worse than degraded", health.StatusBroken, health.StatusDegraded, true},
		{"broken worse than healthy", health.StatusBroken, health.StatusHealthy, true},
		{"degraded worse than healthy", health.StatusDegraded, health.StatusHealthy, true},
		{"healthy not worse than degraded", health.StatusHealthy, health.StatusDegraded, false},
		{"healthy not worse than broken", health.StatusHealthy, health.StatusBroken, false},
		{"degraded not worse than broken", health.StatusDegraded, health.StatusBroken, false},
		{"healthy not worse than healthy", health.StatusHealthy, health.StatusHealthy, false},
		{"degraded not worse than degraded", health.StatusDegraded, health.StatusDegraded, false},
		{"broken not worse than broken", health.StatusBroken, health.StatusBroken, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.WorseThan(tc.o); got != tc.want {
				t.Errorf("%s.WorseThan(%s) = %v, want %v", tc.s, tc.o, got, tc.want)
			}
		})
	}
}

// TestStatusJSONWireFormat proves a health.Status field marshals to the same
// lowercase word as the prior bare string literal, so retyping report structs
// does not change the wire format.
func TestStatusJSONWireFormat(t *testing.T) {
	type report struct {
		Status health.Status `json:"status"`
	}
	cases := []struct {
		s    health.Status
		want string
	}{
		{health.StatusHealthy, `{"status":"healthy"}`},
		{health.StatusDegraded, `{"status":"degraded"}`},
		{health.StatusBroken, `{"status":"broken"}`},
	}
	for _, tc := range cases {
		b, err := json.Marshal(report{Status: tc.s})
		if err != nil {
			t.Fatalf("marshal %s: %v", tc.s, err)
		}
		if string(b) != tc.want {
			t.Errorf("marshal %s = %s, want %s", tc.s, b, tc.want)
		}
		// Round-trip back.
		var r report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.want, err)
		}
		if r.Status != tc.s {
			t.Errorf("round-trip = %s, want %s", r.Status, tc.s)
		}
	}
}
