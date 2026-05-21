package search

import (
	"testing"
)

// TestApplyNameMatchBoost_LiftsNameMatchOverFlatScores covers solov2-x35:
// on a small corpus where vector scores cluster tight, a node whose
// symbol path matches a query token should rank above same-score
// neighbours. Without the boost, all five entries tied at 0.0021 keep
// arbitrary order; with the boost, "NoteStore.Save" lifts above
// "Server", "NewServer", etc. when the query mentions "save".
func TestApplyNameMatchBoost_LiftsNameMatchOverFlatScores(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.00214, SymbolPath: "Server", FilePath: "/x/main.go"},
		{NodeID: "2", Score: 0.00213, SymbolPath: "NewServer", FilePath: "/x/main.go"},
		{NodeID: "3", Score: 0.00212, SymbolPath: "NoteStore.All", FilePath: "/x/store.go"},
		{NodeID: "4", Score: 0.00212, SymbolPath: "NoteStore", FilePath: "/x/store.go"},
		{NodeID: "5", Score: 0.00211, SymbolPath: "NoteStore.Save", FilePath: "/x/store.go"},
	}

	out := applyNameMatchBoost(in, "save a note to memory")
	if len(out) != len(in) {
		t.Fatalf("len changed: %d -> %d", len(in), len(out))
	}

	// NoteStore.Save matches both "save" and "note" → expect rank #1.
	if out[0].NodeID != "5" {
		var got []string
		for _, r := range out {
			got = append(got, r.SymbolPath)
		}
		t.Errorf("top hit = %q, want NoteStore.Save. Order: %v", out[0].SymbolPath, got)
	}
}

// TestApplyNameMatchBoost_TokenFilteringIgnoresShortWords pins that
// 1-2 char tokens ("a", "of", "to") don't drive matches — otherwise
// they'd push everything to the top equally.
func TestApplyNameMatchBoost_TokenFilteringIgnoresShortWords(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.5, SymbolPath: "Alpha", FilePath: "/x/a.go"},
		{NodeID: "2", Score: 0.5, SymbolPath: "Beta", FilePath: "/x/b.go"},
	}
	// Only "a" — too short, no token to match. Order should be preserved.
	out := applyNameMatchBoost(in, "a")
	if out[0].NodeID != "1" {
		t.Errorf("expected stable order with no usable tokens, got %s first", out[0].NodeID)
	}
}

// TestApplyNameMatchBoost_EmptyQueryIsNoop pins the early-exit so the
// boost can never reorder when no query was given (defensive).
func TestApplyNameMatchBoost_EmptyQueryIsNoop(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.9, SymbolPath: "Z", FilePath: "/x/z.go"},
		{NodeID: "2", Score: 0.1, SymbolPath: "A", FilePath: "/x/a.go"},
	}
	out := applyNameMatchBoost(in, "")
	if out[0].NodeID != "1" || out[1].NodeID != "2" {
		t.Errorf("empty query should preserve order; got %s,%s", out[0].NodeID, out[1].NodeID)
	}
}

// TestApplyNameMatchBoost_NoMatchPreservesVectorOrder confirms the
// stable-sort contract: when no token matches anything, the original
// vector-rank order is preserved.
func TestApplyNameMatchBoost_NoMatchPreservesVectorOrder(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.9, SymbolPath: "Z", FilePath: "/x/z.go"},
		{NodeID: "2", Score: 0.8, SymbolPath: "Y", FilePath: "/x/y.go"},
		{NodeID: "3", Score: 0.7, SymbolPath: "X", FilePath: "/x/x.go"},
	}
	out := applyNameMatchBoost(in, "totallyunrelatedwordzzz")
	for i, want := range []string{"1", "2", "3"} {
		if out[i].NodeID != want {
			t.Errorf("rank %d: got %s, want %s", i, out[i].NodeID, want)
		}
	}
}
