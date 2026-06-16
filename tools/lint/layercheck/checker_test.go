package layercheck_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/tools/lint/layercheck"
)

// TestViolationDetection tests the pure violation-detection logic of the layer checker.
func TestViolationDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		importer string
		imported string
		wantViol bool
	}{
		// core/* → application/* is forbidden
		{
			name:     "core imports application is forbidden",
			importer: "github.com/whiskeyjimbo/veska/internal/core/domain",
			imported: "github.com/whiskeyjimbo/veska/internal/application",
			wantViol: true,
		},
		// core/* → infrastructure/* is forbidden
		{
			name:     "core imports infrastructure is forbidden",
			importer: "github.com/whiskeyjimbo/veska/internal/core/domain",
			imported: "github.com/whiskeyjimbo/veska/internal/infrastructure/vector",
			wantViol: true,
		},
		// core/ports → infrastructure/* is forbidden
		{
			name:     "core/ports imports infrastructure is forbidden",
			importer: "github.com/whiskeyjimbo/veska/internal/core/ports",
			imported: "github.com/whiskeyjimbo/veska/internal/infrastructure/vector",
			wantViol: true,
		},
		// application/* → infrastructure/* is forbidden
		{
			name:     "application imports infrastructure is forbidden",
			importer: "github.com/whiskeyjimbo/veska/internal/application",
			imported: "github.com/whiskeyjimbo/veska/internal/infrastructure/vector",
			wantViol: true,
		},
		// application/* → core/ports/* is allowed
		{
			name:     "application imports core/ports is allowed",
			importer: "github.com/whiskeyjimbo/veska/internal/application",
			imported: "github.com/whiskeyjimbo/veska/internal/core/ports",
			wantViol: false,
		},
		// application/* → core/domain/* is allowed
		{
			name:     "application imports core/domain is allowed",
			importer: "github.com/whiskeyjimbo/veska/internal/application",
			imported: "github.com/whiskeyjimbo/veska/internal/core/domain",
			wantViol: false,
		},
		// unrelated packages - no violation
		{
			name:     "infrastructure imports core is fine",
			importer: "github.com/whiskeyjimbo/veska/internal/infrastructure/vector",
			imported: "github.com/whiskeyjimbo/veska/internal/core/ports",
			wantViol: false,
		},
		{
			name:     "third-party import from core is fine",
			importer: "github.com/whiskeyjimbo/veska/internal/core/domain",
			imported: "context",
			wantViol: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := layercheck.IsViolation(tc.importer, tc.imported)
			if got != tc.wantViol {
				t.Errorf("isViolation(%q, %q) = %v, want %v",
					tc.importer, tc.imported, got, tc.wantViol)
			}
		})
	}
}
