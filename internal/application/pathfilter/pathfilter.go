// Package pathfilter centralises the "is this a third-party / vendored path"
// predicate shared by promotion-side checks (dead-code, secret_leak) and
// auto-link. A junior user's first promotion of a freshly-vendored Go CLI
// produced 128 + 232 + 88 noise findings on cobra internals (solov2-l7zd,
// solov2-ttsc); the offending paths all match one of a small set of
// well-known dependency-vendoring directories.
//
// Matching is segment-anywhere on `/`-separated repo-relative paths, so
// monorepo layouts like `apps/foo/vendor/...` are covered too.
package pathfilter

import (
	"slices"
	"strings"
)

// VendoredSegments is the canonical list of path segments that mark a
// dependency-vendoring directory. Kept narrow on purpose: each entry is a
// directory whose contents are third-party code by convention, not a
// directory that users might legitimately populate with their own code.
var VendoredSegments = []string{
	"vendor",           // Go module vendoring
	"node_modules",     // npm/yarn/pnpm
	"third_party",      // common monorepo convention
	"bower_components", // legacy frontend
	"jspm_packages",    // legacy frontend
}

// IsVendored reports whether path lives under any VendoredSegments directory.
// Matching is on full `/`-separated segments, so substring matches (e.g.
// "vendored_data" containing "vendor") do not count. An empty path is treated
// as not-vendored.
func IsVendored(path string) bool {
	if path == "" {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if slices.Contains(VendoredSegments, seg) {
			return true
		}
	}
	return false
}
