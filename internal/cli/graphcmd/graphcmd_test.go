package graphcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// when the chain is empty and chained_selectors_unresolved is
// the degraded reason, the renderer must print an explanatory hint plus a
// pointer to blast/context so users don't read the bare degraded tag as
// "veska is broken".
func TestRenderGraphChain_EmptyWithChainedSelectorsHint(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"nodes":            []any{},
		"edges":            []any{},
		"degraded_reasons": []string{"chained_selectors_unresolved"},
	})
	var buf bytes.Buffer
	if err := RenderGraphChain(context.Background(), &buf, payload, false); err != nil {
		t.Fatalf("RenderGraphChain: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"no nodes in chain",
		"[degraded: chained_selectors_unresolved]",
		"chained selector",
		"veska blast",
		"veska context",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// Other degraded reasons should NOT trigger the chained-selector hint - keep
// the message focused.
func TestRenderGraphChain_EmptyWithOtherDegradedNoHint(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"nodes":            []any{},
		"edges":            []any{},
		"degraded_reasons": []string{"some_other_reason"},
	})
	var buf bytes.Buffer
	if err := RenderGraphChain(context.Background(), &buf, payload, false); err != nil {
		t.Fatalf("RenderGraphChain: %v", err)
	}
	if strings.Contains(buf.String(), "veska blast") {
		t.Errorf("hint leaked for unrelated degraded reason:\n%s", buf.String())
	}
}
