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

import "strings"

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
// An empty path is treated as not-vendored.
func IsVendored(path string) bool {
	if path == "" {
		return false
	}
	for _, seg := range VendoredSegments {
		if hasSegment(path, seg) {
			return true
		}
	}
	return false
}

// hasSegment reports whether seg appears as a full `/`-separated segment of
// path. Substring matches (e.g. "vendored_data" containing "vendor") do not
// count.
func hasSegment(path, seg string) bool {
	for {
		i := strings.Index(path, seg)
		if i < 0 {
			return false
		}
		leftOK := i == 0 || path[i-1] == '/'
		end := i + len(seg)
		rightOK := end == len(path) || path[end] == '/'
		if leftOK && rightOK {
			return true
		}
		path = path[i+len(seg):]
	}
}
