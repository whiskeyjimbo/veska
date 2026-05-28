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

// ReadGoModModulePath parses go.mod and returns the value of its `module`
// directive. An empty path with a nil error indicates no module declaration
// (rare; modfile usually rejects this). Used by eng_list_dependencies to
// filter the repo's own subpackages out of the external-module list
// (solov2-6q1q).
func ReadGoModModulePath(content []byte) (string, error) {
	f, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return "", fmt.Errorf("parse go.mod: %w", err)
	}
	if f.Module == nil {
		return "", nil
	}
	return f.Module.Mod.Path, nil
}
