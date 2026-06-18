// Package pathfilter centralises the "is this a third-party / vendored path"
// predicate shared by promotion-side checks (dead-code, secret_leak) and
// auto-link. A junior user's first promotion of a freshly-vendored Go CLI
// produced 128 + 232 + 88 noise findings on cobra internals (,
// ); the offending paths all match one of a small set of
// well-known dependency-vendoring directories.
// Matching is segment-anywhere on `/`-separated repo-relative paths, so
// monorepo layouts like `apps/foo/vendor/.` are covered too.
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

// IsTestFile reports whether path is a test-shaped source file, by its base
// name, across the languages veska parses. It is the single source of truth for
// the test-file vocabulary, shared by the dead-code / untested-symbol checks
// (which exclude or attribute test files) AND the revalidation sweep's
// test-caller predicate (sqlite.RevalidateRepo.HasTestCaller) - so the
// language-specific naming rules stay in one trivially-testable place rather
// than being duplicated into adapter SQL. An empty path is not a test file.
func IsTestFile(path string) bool {
	if path == "" {
		return false
	}
	base := path
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		base = path[i+1:]
	}
	switch {
	case strings.HasSuffix(base, "_test.go"): // Go
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"): // pytest
		return true
	case strings.HasSuffix(base, "_test.py"): // pytest alt
		return true
	case strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.jsx"),
		strings.HasSuffix(base, ".spec.ts"), strings.HasSuffix(base, ".spec.tsx"),
		strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.jsx"):
		return true
	}
	return false
}
