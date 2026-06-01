package clonescmd

import (
	"strings"
	"testing"
)

func TestRender_Empty(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	render(&b, clonesResp{})
	if got := b.String(); !strings.Contains(got, "no exact clones found") {
		t.Fatalf("empty render = %q, want the no-clones notice", got)
	}
}

func TestRender_GroupsBlock(t *testing.T) {
	t.Parallel()
	var resp clonesResp
	resp.Groups = append(resp.Groups, struct {
		ContentHash string `json:"content_hash"`
		Size        int    `json:"size"`
		Members     []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start"`
		} `json:"members"`
	}{
		ContentHash: "0123456789abcdeffedcba",
		Size:        2,
		Members: []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			LineStart int    `json:"line_start"`
		}{
			{Name: "pkg.A", Kind: "function", FilePath: "a.go", LineStart: 3},
			{Name: "pkg.B", Kind: "function", FilePath: "b.go", LineStart: 7},
		},
	})

	var b strings.Builder
	render(&b, resp)
	out := b.String()

	if !strings.Contains(out, "2 copies (hash 0123456789ab)") {
		t.Errorf("missing header with short hash; got:\n%s", out)
	}
	if !strings.Contains(out, "a.go:3") || !strings.Contains(out, "b.go:7") {
		t.Errorf("missing member locations; got:\n%s", out)
	}
}
