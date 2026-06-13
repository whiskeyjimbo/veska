package findingscmd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestListScopeIncludeSuppressed verifies the --include-suppressed flag flows
// through to the eng_list_findings MCP param include_suppressed=true.
func TestListScopeIncludeSuppressed(t *testing.T) {
	p := ListParams{RepoID: "r1", IncludeSuppressed: true}
	baseParams, _ := p.listScope(context.Background())
	got, ok := baseParams["include_suppressed"]
	if !ok {
		t.Fatalf("include_suppressed missing from baseParams: %v", baseParams)
	}
	if got != true {
		t.Fatalf("include_suppressed = %v, want true", got)
	}
}

// TestListScopeNoIncludeSuppressed verifies the param is omitted by default so
// suppressed findings stay hidden unless explicitly requested.
func TestListScopeNoIncludeSuppressed(t *testing.T) {
	p := ListParams{RepoID: "r1"}
	baseParams, _ := p.listScope(context.Background())
	if _, ok := baseParams["include_suppressed"]; ok {
		t.Fatalf("include_suppressed should be absent by default: %v", baseParams)
	}
}

// TestRenderTableSuppressedMarker verifies a suppressed row renders its
// suppression id and a plain row does not.
func TestRenderTableSuppressedMarker(t *testing.T) {
	sup := "sup_abc123"
	shown := []FindingView{
		{FindingID: "f1", Severity: "high", Rule: "vulnerable_dependency", Message: "boom", SuppressedBy: &sup},
		{FindingID: "f2", Severity: "high", Rule: "dead-code", Message: "ok"},
	}
	var buf bytes.Buffer
	p := ListParams{}
	if err := p.renderTable(&buf, shown); err != nil {
		t.Fatalf("renderTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "SUPPRESSED_BY") {
		t.Fatalf("expected SUPPRESSED_BY header, got:\n%s", out)
	}
	if !strings.Contains(out, sup) {
		t.Fatalf("expected suppression id %q in output, got:\n%s", sup, out)
	}
	// The plain row's line must not carry a suppression id.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "f2") && strings.Contains(line, "sup_") {
			t.Fatalf("plain row f2 should have no suppression id: %q", line)
		}
	}
}

// TestRenderTableLongMessageKeepsSuppressionID guards against the 80-char
// message truncation eating the suppression id (separate column).
func TestRenderTableLongMessageKeepsSuppressionID(t *testing.T) {
	sup := "sup_xyz789"
	long := strings.Repeat("x", 200)
	shown := []FindingView{
		{FindingID: "f1", Severity: "high", Rule: "vulnerable_dependency", Message: long, SuppressedBy: &sup},
	}
	var buf bytes.Buffer
	p := ListParams{}
	if err := p.renderTable(&buf, shown); err != nil {
		t.Fatalf("renderTable: %v", err)
	}
	if !strings.Contains(buf.String(), sup) {
		t.Fatalf("suppression id lost behind message truncation:\n%s", buf.String())
	}
}

// TestFilterLowKeepsSuppressed verifies a low-severity row explicitly surfaced
// by --include-suppressed survives the low-severity filter, while a plain
// low-severity row is still hidden.
func TestFilterLowKeepsSuppressed(t *testing.T) {
	sup := "sup_low1"
	findings := []FindingView{
		{FindingID: "f1", Severity: "low", Rule: "auto-link", SuppressedBy: &sup},
		{FindingID: "f2", Severity: "low", Rule: "auto-link"},
	}
	kept, hiddenLow := ListParams{}.filterLow(findings)
	if hiddenLow != 1 {
		t.Fatalf("hiddenLow = %d, want 1 (only the plain low row hidden)", hiddenLow)
	}
	if len(kept) != 1 || kept[0].FindingID != "f1" {
		t.Fatalf("expected suppressed low row f1 kept, got %+v", kept)
	}
}

// TestFilterLowShownWhenRuleFilter pins solov2-ll57's junior-journey fix: an
// explicit --rule selector surfaces low-severity rows of that rule (e.g.
// dead-code), instead of the confusing empty list a junior got when their only
// finding was low-severity and hidden by the default auto-link-noise filter.
func TestFilterLowShownWhenRuleFilter(t *testing.T) {
	findings := []FindingView{
		{FindingID: "f1", Severity: "low", Rule: "dead-code"},
		{FindingID: "f2", Severity: "low", Rule: "dead-code"},
	}
	kept, hiddenLow := ListParams{Rule: "dead-code"}.filterLow(findings)
	if hiddenLow != 0 {
		t.Fatalf("hiddenLow = %d, want 0 (explicit --rule shows low rows)", hiddenLow)
	}
	if len(kept) != 2 {
		t.Fatalf("expected both low dead-code rows kept under --rule, got %d", len(kept))
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
