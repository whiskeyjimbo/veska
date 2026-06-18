// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/search"
)

type stubPendingCounter struct{ n int }

func (s *stubPendingCounter) CountPending(ctx context.Context) (int, error) {
	return s.n, nil
}

// Compile-time assertion ensures stubPendingCounter implements the PendingEmbedsCounter interface.
var _ PendingEmbedsCounter = (*stubPendingCounter)(nil)

func TestSearchSemantic_PendingTokenStable(t *testing.T) {
	if DegradedReasonEmbeddingsPending != "embeddings_pending" {
		t.Errorf("token drift: %q want embeddings_pending", DegradedReasonEmbeddingsPending)
	}
}

// TestSearchSemantic_DegradedReasonsRoundTripJSON ensures that the DegradedReasons field is correctly serialized to and deserialized from JSON.
func TestSearchSemantic_DegradedReasonsRoundTripJSON(t *testing.T) {
	resp := SearchResponse{
		Results:         []searchHitDTO{},
		DegradedReasons: []string{DegradedReasonEmbeddingsPending, search.DegradedReasonLowQualityStaticEmbedder},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		DegradedReasons []string `json:"degraded_reasons"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.DegradedReasons) != 2 || back.DegradedReasons[0] != "embeddings_pending" {
		t.Errorf("round-trip lost embeddings_pending: %+v", back.DegradedReasons)
	}
}

// TestSearchSemantic_StubReturnsConfiguredCount verifies that the stub counter returns its configured value correctly.
func TestSearchSemantic_StubReturnsConfiguredCount(t *testing.T) {
	pc := &stubPendingCounter{n: 42}
	n, err := pc.CountPending(context.Background())
	if err != nil || n != 42 {
		t.Fatalf("stub returns its configured value; got n=%d err=%v", n, err)
	}
}
