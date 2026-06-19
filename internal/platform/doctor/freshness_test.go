// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import "testing"

func TestCheckIndexFreshness(t *testing.T) {
	head := func(m map[string]string) HeadResolver {
		return func(root string) (string, error) {
			if v, ok := m[root]; ok {
				return v, nil
			}
			return "", errNoHead
		}
	}

	repos := []RepoFreshnessRef{
		{RepoID: "r-current", Branch: "main", RootPath: "/a", PromotedSHA: "sha1"},
		{RepoID: "r-behind", Branch: "main", RootPath: "/b", PromotedSHA: "sha-old"},
		{RepoID: "r-never", Branch: "main", RootPath: "/c", PromotedSHA: ""},
		{RepoID: "r-nogit", Branch: "main", RootPath: "/d", PromotedSHA: "sha9"},
	}
	resolver := head(map[string]string{
		"/a": "sha1",    // matches -> current
		"/b": "sha-new", // differs -> behind
		"/d": "",        // missing -> handled as unknown via error
	})

	got := CheckIndexFreshness(repos, resolver)

	if got.Status != "behind" {
		t.Errorf("overall status = %q, want behind (one repo is behind)", got.Status)
	}
	if got.BehindCount() != 1 {
		t.Errorf("BehindCount = %d, want 1", got.BehindCount())
	}

	want := map[string]string{
		"r-current": "current",
		"r-behind":  "behind",
		"r-never":   "never_promoted",
		"r-nogit":   "unknown",
	}
	for _, rf := range got.Repos {
		if want[rf.RepoID] != rf.State {
			t.Errorf("%s: state = %q, want %q", rf.RepoID, rf.State, want[rf.RepoID])
		}
	}

	// The behind repo must carry the diverging HEAD for the operator.
	for _, rf := range got.Repos {
		if rf.RepoID == "r-behind" && rf.HeadSHA != "sha-new" {
			t.Errorf("r-behind HeadSHA = %q, want sha-new", rf.HeadSHA)
		}
	}
}

func TestCheckIndexFreshness_AllCurrent(t *testing.T) {
	repos := []RepoFreshnessRef{
		{RepoID: "r1", RootPath: "/a", PromotedSHA: "x"},
		{RepoID: "r2", RootPath: "/b", PromotedSHA: "y"},
	}
	resolver := func(root string) (string, error) {
		return map[string]string{"/a": "x", "/b": "y"}[root], nil
	}
	got := CheckIndexFreshness(repos, resolver)
	if got.Status != "current" {
		t.Errorf("status = %q, want current", got.Status)
	}
	if got.BehindCount() != 0 {
		t.Errorf("BehindCount = %d, want 0", got.BehindCount())
	}
}

var errNoHead = errStr("no head")

type errStr string

func (e errStr) Error() string { return string(e) }
