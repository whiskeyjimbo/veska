// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package metric computes statistical series over float samples.
//
// The vocabulary in this module (series, sample, variance, deviation) is
// deliberately disjoint from modbeta's (widget, render, palette, theme) so
// the fuzzy/semantic coverage tools have discriminable signal.
package metric

// Series carries an ordered set of float samples for aggregate computation.
type Series struct {
	Samples []float64
}

// Accumulator incrementally sums samples to derive a running mean.
type Accumulator interface {
	Add(sample float64)
	Mean() float64
}

// ComputeVariance returns the population variance of the series samples.
// TODO: switch to Welford's online algorithm for numerical stability.
func ComputeVariance(s Series) float64 {
	mean := computeMean(s.Samples)
	var acc float64
	for _, sample := range s.Samples {
		delta := sample - mean
		acc += delta * delta
	}
	if len(s.Samples) == 0 {
		return 0
	}
	return acc / float64(len(s.Samples))
}

// computeMean averages the samples; returns 0 for an empty slice.
func computeMean(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	var total float64
	for _, sample := range samples {
		total += sample
	}
	return total / float64(len(samples))
}
