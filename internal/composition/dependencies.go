// SPDX-License-Identifier: AGPL-3.0-only

package composition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/application/manifest"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// NewDependenciesService wires the eng_list_dependencies service: the SQLite
// stub aggregator + import lister, plus go.mod-backed version and own-module
// resolution. Shared by the daemon's tool registration and the in-process
// graph-export composition so the dependency listing is derived through one
// path (AC4: the snapshot reuses this listing rather than re-deriving it).
func NewDependenciesService(pools *sqlite.Pools) (*dependencies.Service, error) {
	depsRepo := sqlite.NewDependenciesRepo(pools.ReadDB)
	repoRoot := RepoRootByID(pools.ReadDB)
	svc, err := dependencies.NewService(depsRepo, goModVersion, repoRoot,
		dependencies.WithImportLister(depsRepo),
		dependencies.WithOwnModulePath(goModOwnModulePath),
	)
	if err != nil {
		return nil, fmt.Errorf("dependencies service: %w", err)
	}
	return svc, nil
}

// goModVersion resolves a module's version from the repo's go.mod. A missing or
// malformed go.mod returns an empty version (the dep still ranks) rather than
// failing the whole List call. Stub rows record the import path (e.g.
// "golang.org/x/text/language") while go.mod lists the module path
// ("golang.org/x/text"), so it walks the path components back until a module
// match falls out, letting sub-packages inherit their parent's version.
func goModVersion(_ context.Context, repoRoot, modulePath string) (string, error) {
	content, rerr := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if rerr != nil {
		return "", nil
	}
	deps, perr := manifest.ReadGoMod(content)
	if perr != nil {
		return "", nil
	}
	probe := modulePath
	for probe != "" && probe != "." {
		for _, m := range deps {
			if m.Name == probe {
				return m.Version, nil
			}
		}
		i := strings.LastIndex(probe, "/")
		if i <= 0 {
			break
		}
		probe = probe[:i]
	}
	return "", nil
}

// goModOwnModulePath returns the repo's own module path so the dependencies
// service can filter intra-module imports (the repo's own subpackages) out of
// the external-dependency list. Absent/malformed go.mod yields "".
func goModOwnModulePath(_ context.Context, repoRoot string) (string, error) {
	content, rerr := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if rerr != nil {
		return "", nil
	}
	path, perr := manifest.ReadGoModModulePath(content)
	if perr != nil {
		return "", nil
	}
	return path, nil
}
