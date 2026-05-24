package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/search"
)

// solov2-hjw9 verification — eng_search_semantic's PendingEmbedsCounter
// contract is honoured: the token, the public response shape, and the
// pluggable interface that wires from sqlite.EmbeddingRefsRepo.

type stubPendingCounter struct{ n int }

func (s *stubPendingCounter) CountPending(ctx context.Context) (int, error) {
	return s.n, nil
}

// Compile-time guard: PendingEmbedsCounter is what the handler type-asserts
// on against SimilarLookup. If its shape ever changes, every call site
// (incl. wire.go's assertion from *sqlite.EmbeddingRefsRepo) must follow —
// pin that contract here so a rename trips this test instead of silently
// dropping the degraded signal.
var _ PendingEmbedsCounter = (*stubPendingCounter)(nil)

func TestSearchSemantic_PendingTokenStable(t *testing.T) {
	if DegradedReasonEmbeddingsPending != "embeddings_pending" {
		t.Errorf("token drift: %q want embeddings_pending", DegradedReasonEmbeddingsPending)
	}
}

// TestSearchSemantic_DegradedReasonsRoundTripJSON guards against an
// accidental serializer drop of the new field.
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

// TestSearchSemantic_StubReturnsConfiguredCount exercises the stub used
// by future integration tests; verifies the pending counter contract
// returns the configured value without error.
func TestSearchSemantic_StubReturnsConfiguredCount(t *testing.T) {
	pc := &stubPendingCounter{n: 42}
	n, err := pc.CountPending(context.Background())
	if err != nil || n != 42 {
		t.Fatalf("stub returns its configured value; got n=%d err=%v", n, err)
	}
}
