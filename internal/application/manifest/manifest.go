// Package manifest reads project dependency manifests and produces the
// ecosystem-tagged dependency set consumed by vulnerability scanning.
//
// The scope is deliberately narrow: only Go module manifests (go.mod) are
// parsed, and parsing is purely textual via golang.org/x/mod/modfile — no
// shell-out to the go toolchain and no network access. This keeps the reader
// a deterministic, offline application-layer service rather than a port.
package manifest

import (
	"fmt"

	"golang.org/x/mod/modfile"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// goEcosystem is the OSV ecosystem identifier for Go modules.
const goEcosystem = "Go"

// ReadGoMod parses the contents of a go.mod file and returns every entry in
// its require block — both direct and // indirect — as []ports.Dependency
// tagged with the "Go" ecosystem. A go.mod with no requires yields an empty,
// non-nil slice and a nil error. A malformed go.mod yields a non-nil error.
func ReadGoMod(content []byte) ([]ports.Dependency, error) {
	f, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.mod: %w", err)
	}

	deps := make([]ports.Dependency, 0, len(f.Require))
	for _, r := range f.Require {
		deps = append(deps, ports.Dependency{
			Ecosystem: goEcosystem,
			Name:      r.Mod.Path,
			Version:   r.Mod.Version,
		})
	}
	return deps, nil
}
