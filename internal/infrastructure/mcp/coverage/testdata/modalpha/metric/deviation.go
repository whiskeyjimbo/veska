// SPDX-License-Identifier: AGPL-3.0-only

package metric

// averageSamples is a near-duplicate of computeMean: same structure, same
// loop, only the local identifiers differ. It exists to give eng_find_clones
// a genuine near-duplicate pair within one module.
func averageSamples(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

// StandardDeviation returns the square-root of the variance of the series.
func StandardDeviation(s Series) float64 {
	v := ComputeVariance(s)
	return sqrtApprox(v)
}

// sqrtApprox is a crude Newton-step square-root used so StandardDeviation has
// a local callee (a within-module CALLS edge target).
func sqrtApprox(x float64) float64 {
	if x <= 0 {
		return 0
	}
	guess := x
	for i := 0; i < 8; i++ {
		guess = 0.5 * (guess + x/guess)
	}
	return guess
}
