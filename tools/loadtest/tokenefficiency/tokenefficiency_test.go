package tokenefficiency

import (
	"reflect"
	"testing"
)

func TestCountTokens_ConsistentForSameString(t *testing.T) {
	t.Parallel()
	s := "the quick brown fox jumps over the lazy dog"
	a, err := CountTokens(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CountTokens(s)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == 0 {
		t.Errorf("CountTokens not consistent or zero: a=%d b=%d", a, b)
	}
}

func TestSimulateGrep_MatchesAnyPhrase(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"a.go":         "validate session tokens via JWT",
		"b.go":         "render markdown to HTML",
		"c.go":         "validate user input shape",
		"unrelated.go": "completely off-topic content",
	}
	got := SimulateGrepFilesWithMatches("validate session tokens. validate user input.", files)
	want := []string{"a.go", "c.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SimulateGrep = %v; want %v", got, want)
	}
}

func TestSimulateGrep_NoMatches(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.go": "x", "b.go": "y"}
	got := SimulateGrepFilesWithMatches("nothing matches here.", files)
	if len(got) != 0 {
		t.Errorf("expected zero matches; got %v", got)
	}
}

func TestBaselineGrep_StopWhenCovered_StopsEarly(t *testing.T) {
	t.Parallel()
	tokens := map[string]int{"a.go": 100, "b.go": 1000, "c.go": 500}
	nodes := map[string][]string{
		"a.go": {"nA1"},
		"b.go": {"nB1", "nB2"}, // truth lives here
		"c.go": {"nC1"},
	}
	truth := map[string]struct{}{"nB1": {}, "nB2": {}}

	lo, hi, loR, hiR := BaselineGrep([]string{"a.go", "b.go", "c.go"}, tokens, nodes, truth)
	// Lo: reads a.go (100) + b.go (1000) and stops because b covers truth.
	// Hi: reads everything = 1600.
	if lo != 1100 {
		t.Errorf("lo tokens = %d; want 1100", lo)
	}
	if hi != 1600 {
		t.Errorf("hi tokens = %d; want 1600", hi)
	}
	if loR != 1 {
		t.Errorf("lo recall = %v; want 1.0 (covered)", loR)
	}
	if hiR != 1 {
		t.Errorf("hi recall = %v; want 1.0 (all truth in matched files)", hiR)
	}
}

func TestBaselineGrep_EmptyHits_NoTokens(t *testing.T) {
	t.Parallel()
	lo, hi, loR, hiR := BaselineGrep(nil, nil, nil, map[string]struct{}{"x": {}})
	if lo != 0 || hi != 0 || loR != 0 || hiR != 0 {
		t.Errorf("empty hits should yield zeroes; got lo=%d hi=%d loR=%v hiR=%v", lo, hi, loR, hiR)
	}
}

func TestRecallAtK_StandardSemantics(t *testing.T) {
	t.Parallel()
	truth := map[string]struct{}{"a": {}, "b": {}, "c": {}}
	if got := RecallAtK([]string{"a", "b", "c", "d"}, truth, 10); got != 1.0 {
		t.Errorf("all-three-hit recall = %v; want 1.0", got)
	}
	if got := RecallAtK([]string{"a", "x", "y"}, truth, 3); got < 0.33 || got > 0.34 {
		t.Errorf("one-of-three recall@3 = %v; want ~0.333", got)
	}
	if got := RecallAtK(nil, truth, 10); got != 0 {
		t.Errorf("no hits recall = %v; want 0", got)
	}
}

func TestSavingsRatio_NegativeWhenVeskaWorse(t *testing.T) {
	t.Parallel()
	if got := SavingsRatio(200, 100); got != -1.0 {
		t.Errorf("veska 2x grep savings = %v; want -1.0", got)
	}
	if got := SavingsRatio(50, 100); got != 0.5 {
		t.Errorf("veska half grep savings = %v; want 0.5", got)
	}
	if got := SavingsRatio(50, 0); got != 0 {
		t.Errorf("zero-baseline savings = %v; want 0", got)
	}
}

func TestFillAbsoluteSavings(t *testing.T) {
	t.Parallel()
	r := Result{
		MeanVeskaTokens:  100,
		MeanGrepLoTokens: 400,
		MeanGrepHiTokens: 600,
	}
	r.FillAbsoluteSavings(50, 3.0, "test rate")
	// midpoint(400,600)=500; saved=500-100=400 per query.
	if r.TokensSavedPerQuery != 400 {
		t.Errorf("TokensSavedPerQuery = %v; want 400", r.TokensSavedPerQuery)
	}
	if r.TokensSavedOverConversation != 20000 {
		t.Errorf("conversation tokens = %v; want 20000", r.TokensSavedOverConversation)
	}
	// 20,000 tokens * $3/M = $0.06.
	if r.USDSavedOverConversation < 0.0599 || r.USDSavedOverConversation > 0.0601 {
		t.Errorf("USD = %v; want ~0.06", r.USDSavedOverConversation)
	}
	if r.USDPriceLabel != "test rate" {
		t.Errorf("label = %q", r.USDPriceLabel)
	}
}

func TestFillAbsoluteSavings_NegativeClampedToZero(t *testing.T) {
	t.Parallel()
	// Pathological: veska used MORE tokens than grep (shouldn't
	// happen on real corpora but is a clean clamp test).
	r := Result{
		MeanVeskaTokens:  1000,
		MeanGrepLoTokens: 100,
		MeanGrepHiTokens: 200,
	}
	r.FillAbsoluteSavings(50, 3.0, "x")
	if r.TokensSavedPerQuery != 0 {
		t.Errorf("expected 0 (clamped), got %v", r.TokensSavedPerQuery)
	}
}

func TestFormatThousands(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		0: "0", 7: "7", 100: "100", 999: "999",
		1000: "1,000", 12300: "12,300", 1234567: "1,234,567",
		-12300: "-12,300",
	}
	for in, want := range cases {
		if got := formatThousands(in); got != want {
			t.Errorf("formatThousands(%d) = %q; want %q", in, got, want)
		}
	}
}

func TestSummaryLine_ExpectedShape(t *testing.T) {
	t.Parallel()
	r := Result{
		Queries:             100,
		MeanRecall:          0.65,
		MeanVeskaTokens:     200,
		MeanGrepLoTokens:    1000,
		MeanGrepHiTokens:    4000,
		MeanSavingsLoVsGrep: 0.80,
		MeanSavingsHiVsGrep: 0.95,
	}
	got := r.SummaryLine()
	// Spot-check the structural words from the expected summary layout.
	for _, want := range []string{"Veska found", "65%", "80", "95", "100 queries"} {
		if !contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
