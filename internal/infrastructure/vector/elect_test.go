// SPDX-License-Identifier: AGPL-3.0-only

package vector_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

func TestElectVectorBackend(t *testing.T) {
	const thr = vector.AutoElectThreshold
	cases := []struct {
		name       string
		configured vector.BackendKind
		maxVec     int
		usearch    bool
		want       vector.BackendKind
	}{
		{"explicit memory stays memory even when huge", vector.BackendMemory, thr * 100, true, vector.BackendMemory},
		{"explicit usearch stays usearch even when tiny", vector.BackendUsearch, 0, true, vector.BackendUsearch},
		{"auto + below threshold -> memory", vector.BackendAuto, thr - 1, true, vector.BackendMemory},
		{"auto + at threshold + available -> usearch", vector.BackendAuto, thr, true, vector.BackendUsearch},
		{"auto + above threshold + available -> usearch", vector.BackendAuto, thr * 5, true, vector.BackendUsearch},
		{"auto + above threshold but unavailable -> memory", vector.BackendAuto, thr * 5, false, vector.BackendMemory},
		{"auto + empty graph -> memory", vector.BackendAuto, 0, true, vector.BackendMemory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vector.ElectVectorBackend(tc.configured, tc.maxVec, tc.usearch)
			if got != tc.want {
				t.Errorf("ElectVectorBackend(%q, %d, %v) = %q, want %q",
					tc.configured, tc.maxVec, tc.usearch, got, tc.want)
			}
			if got == vector.BackendAuto {
				t.Errorf("result must be concrete, got BackendAuto")
			}
		})
	}
}
