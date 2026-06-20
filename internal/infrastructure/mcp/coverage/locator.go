// SPDX-License-Identifier: AGPL-3.0-only

package coverage

import (
	"path/filepath"
	"runtime"
)

// ModuleRoot returns the absolute filesystem path to a fixture module directory
// under this package's testdata/ tree (e.g. ModuleRoot("modalpha")).
// It is anchored via runtime.Caller rather than a cwd-relative "testdata/."
// join so importers in OTHER packages (notably the in-process tool-coverage
// harness in package daemon) can locate the fixtures regardless of their own
// working directory. The dump/self-test helpers in this package use a
// cwd-relative path because their cwd is always this package.
func ModuleRoot(module string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "testdata", module)
}
