// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package clonescmd

import (
	"strings"
	"testing"
)

func TestRender_EmptyExact(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	render(&b, clonesResp{Mode: "exact"})
	if got := b.String(); !strings.Contains(got, "no exact clones found") {
		t.Fatalf("empty exact render = %q, want the no-clones notice", got)
	}
}

func TestRender_EmptyNear(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	render(&b, clonesResp{Mode: "near"})
	got := b.String()
	if !strings.Contains(got, "no near-duplicate clusters found") {
		t.Fatalf("empty near render = %q, want the no-clusters notice", got)
	}
	if !strings.Contains(got, "reindex") {
		t.Errorf("near empty render should hint at reindex; got %q", got)
	}
}

func TestRender_GroupsBlock(t *testing.T) {
	t.Parallel()
	resp := clonesResp{Mode: "exact"}
	resp.Groups = append(resp.Groups, struct {
		ContentHash string        `json:"content_hash"`
		Size        int           `json:"size"`
		Members     []cloneMember `json:"members"`
	}{
		ContentHash: "0123456789abcdeffedcba",
		Size:        2,
		Members: []cloneMember{
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

func TestRender_ClustersBlock(t *testing.T) {
	t.Parallel()
	resp := clonesResp{Mode: "near"}
	resp.Clusters = append(resp.Clusters, struct {
		Size     int           `json:"size"`
		MinScore float32       `json:"min_score"`
		MaxScore float32       `json:"max_score"`
		Members  []cloneMember `json:"members"`
	}{
		Size:     2,
		MinScore: 0.812,
		MaxScore: 0.954,
		Members: []cloneMember{
			{Name: "pkg.A", Kind: "function", FilePath: "a.go", LineStart: 3},
			{Name: "pkg.B", Kind: "function", FilePath: "b.go", LineStart: 7},
		},
	})

	var b strings.Builder
	render(&b, resp)
	out := b.String()

	if !strings.Contains(out, "2 similar (score 0.812–0.954)") {
		t.Errorf("missing cluster header with score range; got:\n%s", out)
	}
	if !strings.Contains(out, "a.go:3") || !strings.Contains(out, "b.go:7") {
		t.Errorf("missing member locations; got:\n%s", out)
	}
}
