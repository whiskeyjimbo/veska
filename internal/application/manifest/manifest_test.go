// SPDX-License-Identifier: AGPL-3.0-only

package manifest_test

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/manifest"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

func TestReadGoMod(t *testing.T) {
	tests := []struct {
		name    string
		gomod   string
		want    []ports.Dependency
		wantErr bool
	}{
		{
			name: "direct and indirect requires",
			gomod: `module example.com/app

go 1.26

require (
	golang.org/x/mod v0.36.0
	github.com/foo/bar v1.2.3 // indirect
)
`,
			want: []ports.Dependency{
				{Ecosystem: "Go", Name: "golang.org/x/mod", Version: "v0.36.0"},
				{Ecosystem: "Go", Name: "github.com/foo/bar", Version: "v1.2.3"},
			},
		},
		{
			name: "single-line require directives",
			gomod: `module example.com/app

go 1.26

require golang.org/x/sys v0.20.0
require github.com/baz/qux v0.1.0 // indirect
`,
			want: []ports.Dependency{
				{Ecosystem: "Go", Name: "golang.org/x/sys", Version: "v0.20.0"},
				{Ecosystem: "Go", Name: "github.com/baz/qux", Version: "v0.1.0"},
			},
		},
		{
			name: "no requires returns empty slice",
			gomod: `module example.com/app

go 1.26
`,
			want: []ports.Dependency{},
		},
		{
			name:    "malformed go.mod returns error",
			gomod:   "this is not a valid go.mod {{{",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := manifest.ReadGoMod([]byte(tt.gomod))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
					return
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d deps, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("dep[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
