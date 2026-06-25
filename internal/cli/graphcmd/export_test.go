// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

func TestRepoContaining(t *testing.T) {
	outer := repo.Record{RepoID: "outer", RootPath: filepath.FromSlash("/src/proj")}
	inner := repo.Record{RepoID: "inner", RootPath: filepath.FromSlash("/src/proj/vendored")}
	other := repo.Record{RepoID: "other", RootPath: filepath.FromSlash("/work/elsewhere")}
	recs := []repo.Record{outer, inner, other}

	cases := []struct {
		name   string
		cwd    string
		wantID string
		wantOK bool
	}{
		{"exact root", filepath.FromSlash("/src/proj"), "outer", true},
		{"under root", filepath.FromSlash("/src/proj/internal/x"), "outer", true},
		{"nested prefers longest root", filepath.FromSlash("/src/proj/vendored/pkg"), "inner", true},
		{"no match", filepath.FromSlash("/tmp/unrelated"), "", false},
		{"prefix-but-not-subdir", filepath.FromSlash("/src/projector"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repoContaining(recs, tc.cwd)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.RepoID != tc.wantID {
				t.Errorf("repo = %q, want %q", got.RepoID, tc.wantID)
			}
		})
	}
}
