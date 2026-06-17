// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package static_test

import (
	"context"
	"math"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
)

// TestProvider_Deterministic: the same input must produce the same
// vector across runs, processes, and machines - otherwise the
// content_hash + model_id embedding cache would silently miss every
// re-run and re-embed every node.
func TestProvider_Deterministic(t *testing.T) {
	p, err := static.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v1, err := p.Embed(context.Background(), "func parseConfig() error")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	v2, err := p.Embed(context.Background(), "func parseConfig() error")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v1) != len(v2) {
		t.Fatalf("dim differs: %d vs %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("vec differs at %d: %v vs %v", i, v1[i], v2[i])
			break
		}
	}
}

// TestProvider_DimensionMatchesNomic: 768 matches nomic-embed-text so
// the static embedder is a drop-in replacement at the vector-storage
// layer - no schema or migration needed.
func TestProvider_DimensionMatchesNomic(t *testing.T) {
	p, _ := static.New()
	v, err := p.Embed(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != 768 {
		t.Errorf("dim = %d, want 768", len(v))
	}
}

// TestProvider_L2Normalized: search scores derive from cosine-shaped
// math (1 / (1 + L2)) and assume unit-length vectors; an
// un-normalised embedder would silently skew rankings.
func TestProvider_L2Normalized(t *testing.T) {
	p, _ := static.New()
	v, _ := p.Embed(context.Background(), "func parseConfig() error")
	var sumsq float64
	for _, x := range v {
		sumsq += float64(x) * float64(x)
	}
	mag := math.Sqrt(sumsq)
	if math.Abs(mag-1.0) > 1e-5 {
		t.Errorf("vector not L2-normalised: |v| = %v", mag)
	}
}

// TestProvider_DifferentInputsProduceDifferentVectors: with high
// probability, two distinct inputs hash to distinct vectors. Without
// this, every node would land on the same point in vector space and
// search would be useless.
func TestProvider_DifferentInputsProduceDifferentVectors(t *testing.T) {
	p, _ := static.New()
	a, _ := p.Embed(context.Background(), "func parseConfig() error")
	b, _ := p.Embed(context.Background(), "type Server struct{}")
	identical := true
	for i := range a {
		if a[i] != b[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Error("distinct inputs produced identical vectors")
	}
}

// TestProvider_EmptyInputDoesNotPanic: tokenising an empty (or
// whitespace-only) string yields zero tokens; the embedder must
// still return a finite, L2-normalised vector instead of NaN or 0/0.
func TestProvider_EmptyInputDoesNotPanic(t *testing.T) {
	p, _ := static.New()
	v, err := p.Embed(context.Background(), "")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(v) != 768 {
		t.Errorf("dim = %d", len(v))
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Errorf("non-finite component: %v", x)
		}
	}
}

// TestProvider_SubwordSimilarity_BeatsUnrelated covers the central
// quality upgrade from v1 (per-token hash) to v2 (character-n-gram
// hashing, FastText-style): identifiers that share subword morphology
// should land closer in vector space than unrelated identifiers.
// Without this property the static embedder cannot retrieve
// "configParser" when the query mentions "parseConfig" - the exact
// failure mode that motivated the quality follow-up.
func TestProvider_SubwordSimilarity_BeatsUnrelated(t *testing.T) {
	p, _ := static.New()
	a, _ := p.Embed(context.Background(), "parseConfig")
	b, _ := p.Embed(context.Background(), "configParser")
	c, _ := p.Embed(context.Background(), "renderTemplate")

	abSim := cosine(a, b)
	acSim := cosine(a, c)
	if abSim <= acSim {
		t.Errorf("subword-sharing identifiers should be more similar than unrelated; cos(parseConfig,configParser)=%v cos(parseConfig,renderTemplate)=%v",
			abSim, acSim)
	}
}

func cosine(a, b []float32) float64 {
	// Both vectors are L2-normalised, so cosine = dot product.
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// TestProvider_ModelIDStable: the model identifier participates in
// the embedding cache key (nodes refresh when it changes), so it
// must NOT include user input or system state.
func TestProvider_ModelIDStable(t *testing.T) {
	p, _ := static.New()
	if id := p.ModelID(); id == "" {
		t.Error("ModelID is empty")
	}
	if got := p.ModelID(); got != p.ModelID() {
		t.Errorf("ModelID changes across calls: %q vs %q", got, p.ModelID())
	}
}
