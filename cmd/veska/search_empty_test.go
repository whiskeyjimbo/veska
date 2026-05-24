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
