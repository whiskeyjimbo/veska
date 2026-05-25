package main

import (
	"bytes"
	"strings"
	"testing"
)

// solov2-ffi3 verification — renderSearchEnvelope must print SOMETHING
// when there are no results, so a junior user can tell "ran with no
// hits" from "broken / misconfigured". The exact wording follows the
// pendingEmbedsHint signal, but the contract is: non-empty text output
// for an empty envelope.

func TestRenderSearchEnvelope_EmptyTextPrintsNoResults(t *testing.T) {
	var w bytes.Buffer
	if err := renderSearchEnvelope(&w, searchEnvelope{Results: []searchHitView{}}, false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := w.String()
	if out == "" {
		t.Fatal("empty text-mode result must print something; got empty output")
	}
	if !strings.Contains(out, "no results") {
		t.Errorf("expected output to contain 'no results'; got %q", out)
	}
}

func TestRenderSearchEnvelope_EmptyJSONStillEmitsResultsArray(t *testing.T) {
	var w bytes.Buffer
	if err := renderSearchEnvelope(&w, searchEnvelope{Results: nil}, true); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(w.String(), `"results"`) || !strings.Contains(w.String(), `[]`) {
		t.Errorf("JSON mode must always carry an empty results array; got %s", w.String())
	}
}

func TestRenderSearchEnvelope_DegradedReasonsSurfacedWithEmpty(t *testing.T) {
	var w bytes.Buffer
	env := searchEnvelope{
		Results:         []searchHitView{},
		DegradedReasons: []string{"embeddings_pending"},
	}
	if err := renderSearchEnvelope(&w, env, false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(w.String(), "embeddings_pending") {
		t.Errorf("empty-result path must still echo degraded_reasons; got %q", w.String())
	}
}

// TestRenderSearchEnvelope_DegradedHintAppendsToCode guards solov2-0qk5: a
// raw "[degraded: <code>]" line is opaque to a new user. The renderer
// appends a short actionable hint per known code (low_quality_static_embedder
// and no_post_registration_commits don't need a daemon to look up — they
// are static guidance).
func TestRenderSearchEnvelope_DegradedHintAppendsToCode(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		expect string
	}{
		{
			name:   "low_quality_static_embedder",
			code:   "low_quality_static_embedder",
			expect: "veska install model2vec",
		},
		{
			name:   "no_post_registration_commits",
			code:   "no_post_registration_commits",
			expect: "commits land while the repo is registered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w bytes.Buffer
			env := searchEnvelope{
				Results: []searchHitView{{
					NodeID: "n1", Name: "X", Kind: "function", FilePath: "x.go",
					LineStart: 1, LineEnd: 2, Score: 0.42,
				}},
				DegradedReasons: []string{tc.code},
			}
			if err := renderSearchEnvelope(&w, env, false); err != nil {
				t.Fatalf("render: %v", err)
			}
			got := w.String()
			if !strings.Contains(got, tc.code) {
				t.Errorf("missing raw code %q in %q", tc.code, got)
			}
			if !strings.Contains(got, tc.expect) {
				t.Errorf("missing hint %q in %q", tc.expect, got)
			}
		})
	}
}

// TestDegradedReasonHint_UnknownIsEmpty: an unknown code passes through with
// no appended hint (preserves forward-compat with new server-side codes).
func TestDegradedReasonHint_UnknownIsEmpty(t *testing.T) {
	if got := degradedReasonHint("mystery_code"); got != "" {
		t.Errorf("unknown code should produce empty hint, got %q", got)
	}
}

// TestRenderSearchEnvelope_LowAbsoluteTopAppendsNote guards solov2-gfhq: when
// the top hit's absolute score is below weakTopAbsolute, the renderer prints a
// one-line note explaining the tier labels are relative. Without this, a 0.018
// "top" looks confidently correct.
func TestRenderSearchEnvelope_LowAbsoluteTopAppendsNote(t *testing.T) {
	var w bytes.Buffer
	// 0.0164 corresponds to a single-retriever rank-1 RRF hit (1/(60+1)),
	// the exact case the warning is meant to catch.
	env := searchEnvelope{
		Results: []searchHitView{{
			NodeID: "n1", Name: "X", Kind: "function", FilePath: "x.go",
			LineStart: 1, LineEnd: 2, Score: 0.0164,
		}},
	}
	if err := renderSearchEnvelope(&w, env, false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(w.String(), "top match score is low") {
		t.Errorf("expected weak-top note in %q", w.String())
	}
}

// TestRenderSearchEnvelope_HealthyTopOmitsNote: the note is only printed when
// the top is below the floor — confident matches should be quiet.
func TestRenderSearchEnvelope_HealthyTopOmitsNote(t *testing.T) {
	var w bytes.Buffer
	env := searchEnvelope{
		Results: []searchHitView{{
			NodeID: "n1", Name: "X", Kind: "function", FilePath: "x.go",
			LineStart: 1, LineEnd: 2, Score: 0.42,
		}},
	}
	if err := renderSearchEnvelope(&w, env, false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(w.String(), "top match score is low") {
		t.Errorf("did not expect weak-top note for healthy score; got %q", w.String())
	}
}
