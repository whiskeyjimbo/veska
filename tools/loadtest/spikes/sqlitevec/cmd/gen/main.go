// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Command gen generates synthetic 768-dim float32 vectors with L2 norms drawn
// from Gaussian(μ=7.5, σ=1.5) and writes them to disk in the vecbin format.
// Usage:
//
//	gen -n 50000 -out data/vectors.bin
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/gen"
	"github.com/whiskeyjimbo/veska/tools/loadtest/spikes/sqlitevec/vecbin"
)

func main() {
	n := flag.Int("n", 50000, "number of vectors to generate")
	out := flag.String("out", "data/vectors.bin", "output file path")
	flag.Parse()

	seed := rand.Uint64()
	fmt.Printf("Generating %d vectors (seed=%d)...\n", *n, seed)

	vecs := gen.GenerateVectors(*n, seed)

	stats := gen.ComputeStats(vecs)
	fmt.Printf("Norm stats - mean: %.4f  p50: %.4f  p95: %.4f\n", stats.Mean, stats.P50, stats.P95)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(*out), err)
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create %s: %v", *out, err)
	}
	defer f.Close()

	if err := vecbin.WriteVectors(f, vecs); err != nil {
		log.Fatalf("write vectors: %v", err)
	}
	fmt.Printf("Written %d vectors to %s\n", len(vecs), *out)
}
