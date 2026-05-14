// Package autolink contains the eval harness for veska's auto-link
// candidate computation (internal/application/autolink.Linker). Pure
// FP-rate math lives here without a build tag so it compiles under
// default `go test`/`go vet` and is unit-testable in isolation; the
// end-to-end eval driver that wires up a real Linker against a real
// VectorStorage + SQLite is gated by the `eval` build tag.
package autolink

// Pair is a single (source, target) classification record passed to
// FPRate. The pure math layer never sees the cluster integers
// directly — the caller folds the cluster lookup into TruePositive
// (true iff src and tgt are in the same cluster).
type Pair struct {
	TruePositive bool
}

// FPRate returns the false-positive rate over a batch of emitted
// candidate pairs.
//
// Semantics:
//   - FP rate is defined as false_positives / total_emitted_pairs.
//   - A "false positive" is a pair where TruePositive == false.
//   - Empty input is by convention 0 (no signal, no error).
//   - The result is bounded in [0, 1] by construction: every pair is
//     either TP or FP, so the count of FPs is at most the total.
//
// The complementary TP/FP counts are exposed by FPCounts so callers
// can write a richer summary line without recomputing the ratio.
func FPRate(pairs []Pair) float64 {
	if len(pairs) == 0 {
		return 0
	}
	fp, _ := FPCounts(pairs)
	return float64(fp) / float64(len(pairs))
}

// FPCounts returns (false_positive_count, true_positive_count) for the
// supplied pairs. Empty input returns (0, 0).
func FPCounts(pairs []Pair) (fp, tp int) {
	for _, p := range pairs {
		if p.TruePositive {
			tp++
		} else {
			fp++
		}
	}
	return fp, tp
}
