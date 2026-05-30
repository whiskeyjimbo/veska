package searchcmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/search"
)

// TestIsGitURL covers the heuristic that routes a positional arg to
// the clone path vs the local-path path. False negatives are loud (a
// path with a "://" in it would route to clone — but no real filesystem
// path looks like that).
func TestIsGitURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://github.com/foo/bar", true},
		{"http://example.com/x", true},
		{"git@github.com:foo/bar.git", true},
		{"/tmp/some/path", false},
		{"./relative", false},
		{"~/home/path", false},
		{"foo-bar", false},
	}
	for _, c := range cases {
		if got := IsGitURL(c.in); got != c.want {
			t.Errorf("IsGitURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestRenderSearchResults_JSONMatchesMCPEnvelope covers AC3: the
// stdout JSON shape must match what the eng_search_semantic MCP tool
// emits — a top-level {results, degraded_reasons} envelope keyed
// exactly that way so agents can pipe the CLI output through the
// same parser they use for tool responses.
func TestRenderSearchResults_JSONMatchesMCPEnvelope(t *testing.T) {
	var buf bytes.Buffer
	resp := search.Response{
		Results: []search.Result{
			{NodeID: "n1", Score: 0.7, SymbolPath: "pkg.Foo", FilePath: "a.go", Kind: "function", LineStart: 10, LineEnd: 12},
		},
		DegradedReasons: []string{"embedder_offline_lexical_fallback"},
	}
	if err := RenderSearchResults(&buf, resp, true); err != nil {
		t.Fatalf("renderSearchResults: %v", err)
	}
	var got struct {
		Results         []map[string]any `json:"results"`
		DegradedReasons []string         `json:"degraded_reasons"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, buf.String())
	}
	if len(got.Results) != 1 {
		t.Fatalf("results count: got %d, want 1", len(got.Results))
	}
	// The CLI emits the same snake_case node DTO as eng_search_semantic
	// (solov2-elt): node_id, name, file_path, ...
	if got.Results[0]["node_id"] != "n1" {
		t.Errorf("node_id field missing or wrong: %+v", got.Results[0])
	}
	if got.Results[0]["name"] != "pkg.Foo" {
		t.Errorf("name field missing or wrong: %+v", got.Results[0])
	}
	if len(got.DegradedReasons) != 1 || got.DegradedReasons[0] != "embedder_offline_lexical_fallback" {
		t.Errorf("degraded_reasons not preserved: %+v", got.DegradedReasons)
	}
}

// TestRenderSearchResults_HumanFormatIncludesKey: the non-JSON
// fallback should be greppable — the symbol path + line range belong
// on one line so the user can pipe through grep without losing
// context.
func TestRenderSearchResults_HumanFormatIncludesKey(t *testing.T) {
	var buf bytes.Buffer
	resp := search.Response{
		Results: []search.Result{
			{NodeID: "n1", Score: 0.7, SymbolPath: "pkg.Foo", FilePath: "a.go", Kind: "function", LineStart: 10, LineEnd: 12},
		},
	}
	if err := RenderSearchResults(&buf, resp, false); err != nil {
		t.Fatalf("renderSearchResults: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"pkg.Foo", "a.go", "10-12", "function"} {
		if !strings.Contains(out, want) {
			t.Errorf("human format missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderSearchResults_EmptyResultsJSONHasResultsKey: even on zero
// hits the JSON must emit "results": [] (not omitted). Agents
// parsing the envelope expect the key.
func TestRenderSearchResults_EmptyResultsJSONHasResultsKey(t *testing.T) {
	var buf bytes.Buffer
	resp := search.Response{}
	if err := RenderSearchResults(&buf, resp, true); err != nil {
		t.Fatalf("renderSearchResults: %v", err)
	}
	if !strings.Contains(buf.String(), `"results": []`) {
		t.Errorf("empty result set should emit explicit []: %s", buf.String())
	}
}
