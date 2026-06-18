// Package tokenefficiency is the eval harness that produces veska's
// "tokens saved vs grep+read" benchmark figure. It is
// distinct from internal/savings - which reports a live char-ratio
// alongside every search - and the two MUST NOT be conflated in
// user-facing output. The benchmark is suitable for docs / READMEs /
// the appendix of the cross-repo eval; the live char-ratio stays
// pure operational telemetry.
// The pure functions live in tokenefficiency.go (no build tag). The
// end-to-end driver that wires up a real search.Service lives in
// bench_test.go behind the eval build tag.
package tokenefficiency
