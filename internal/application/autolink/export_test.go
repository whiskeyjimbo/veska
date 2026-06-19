// SPDX-License-Identifier: AGPL-3.0-only

package autolink

// AutolinkNoiseForTest exposes the unexported isIdiomaticAutolinkNoise
// predicate so the external _test package can drive its truth table.
func AutolinkNoiseForTest(srcSym, tgtSym, srcKind, tgtKind string) bool {
	return isIdiomaticAutolinkNoise(srcSym, tgtSym, srcKind, tgtKind)
}
