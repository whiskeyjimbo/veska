// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import "testing"

// TestOptionsVerifyIntegrityResolution pins the precedence of the schema-tamper
// gate: off by default, on when the config field is set, and a non-empty
// VESKA_MIGRATION_INTEGRITY env var overriding the field either way so an
// operator always has a disable/enable escape hatch. Unset or empty defers to
// the field.
func TestOptionsVerifyIntegrityResolution(t *testing.T) {
	tests := []struct {
		name  string
		field bool
		env   string
		want  bool
	}{
		{name: "default off", field: false, env: "", want: false},
		{name: "config field on", field: true, env: "", want: true},
		{name: "empty env defers to field off", field: false, env: "", want: false},
		{name: "empty env defers to field on", field: true, env: "", want: true},
		{name: "env 1 forces on over field off", field: false, env: "1", want: true},
		{name: "env true forces on over field off", field: false, env: "true", want: true},
		{name: "env TRUE case-insensitive", field: false, env: "TRUE", want: true},
		{name: "env 0 forces off over field on", field: true, env: "0", want: false},
		{name: "env garbage forces off over field on", field: true, env: "nope", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VESKA_MIGRATION_INTEGRITY", tt.env)
			if got := (Options{VerifyIntegrity: tt.field}).verifyIntegrity(); got != tt.want {
				t.Fatalf("verifyIntegrity() = %v, want %v", got, tt.want)
			}
		})
	}
}
