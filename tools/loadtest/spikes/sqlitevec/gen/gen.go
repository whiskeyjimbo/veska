// Package gen provides synthetic vector generation with realistic L2 norms
// matching the empirical distribution of nomic-embed-text embeddings.
// Each vector is drawn by:
//  1. Sampling a 768-dimensional direction uniform on the unit sphere via
//     Box-Muller (draw standard normals, normalize).
//  2. Scaling by a magnitude drawn from Gaussian(μ=7.5, σ=1.5), rejecting
//     non-positive samples.
package gen

import (
	"math"
	"math/rand/v2"
	"sort"
)

const (
	Dim     = 768
	MuNorm  = 7.5
	SigNorm = 1.5
)

// Stats holds summary statistics for L2 norms of a vector set.
type Stats struct {
	Mean float64
	P50  float64
	P95  float64
}

// GenerateVectors returns n synthetic 768-dim float32 vectors.
// seed is used to seed a deterministic PRNG (PCG via math/rand/v2).
func GenerateVectors(n int, seed uint64) [][]float32 {
	rng := rand.New(rand.NewPCG(seed, seed^0xdeadbeef))
	vecs := make([][]float32, 0, n)
	buf := make([]float64, Dim)

	for len(vecs) < n {
		var sumSq float64
		for i := range buf {
			buf[i] = randn(rng)
			sumSq += buf[i] * buf[i]
		}
		if sumSq == 0 {
			continue
		}
		invLen := 1.0 / math.Sqrt(sumSq)

		mag := MuNorm + SigNorm*randn(rng)
		if mag <= 0 {
			continue
		}

		v := make([]float32, Dim)
		for i, d := range buf {
			v[i] = float32(d * invLen * mag)
		}
		vecs = append(vecs, v)
	}
	return vecs
}

// randn returns one standard normal variate using the Box-Muller transform.
func randn(rng *rand.Rand) float64 {
	u1 := rng.Float64()
	u2 := rng.Float64()
	for u1 == 0 {
		u1 = rng.Float64()
	}
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// ComputeStats computes mean, p50, and p95 L2 norms for vecs.
func ComputeStats(vecs [][]float32) Stats {
	norms := make([]float64, len(vecs))
	var sum float64
	for i, v := range vecs {
		var sq float64
		for _, f := range v {
			sq += float64(f) * float64(f)
		}
		n := math.Sqrt(sq)
		norms[i] = n
		sum += n
	}
	sort.Float64s(norms)
	m := len(norms)
	return Stats{
		Mean: sum / float64(m),
		P50:  norms[m/2],
		P95:  norms[int(float64(m)*0.95)],
	}
}
