// SPDX-License-Identifier: AGPL-3.0-only

package diffgate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
)

func TestRefChangeSource_ReadsChangedFilesAtCandidate(t *testing.T) {
	changed := func(_ context.Context, root, a, b string) ([]string, error) {
		if root != "/repo" || a != "base" || b != "cand" {
			t.Fatalf("changedFiles got (%s,%s,%s)", root, a, b)
		}
		return []string{"a.go", "gone.go"}, nil
	}
	at := func(_ context.Context, _, ref, path string) ([]byte, error) {
		if ref != "cand" {
			t.Fatalf("fileAtRef should read the candidate ref, got %s", ref)
		}
		switch path {
		case "a.go":
			return []byte("package a"), nil
		case "gone.go":
			// Deleted in the candidate: sentinel-wrapped absence.
			return nil, fmt.Errorf("%w: gone.go", diffgate.ErrFileAbsentAtRef)
		}
		t.Fatalf("unexpected path %s", path)
		return nil, nil
	}

	src, err := diffgate.NewRefChangeSource("/repo", "base", "cand", changed, at)
	if err != nil {
		t.Fatalf("NewRefChangeSource: %v", err)
	}
	changes, err := src.Changes(context.Background())
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(changes))
	}
	if changes[0].Path != "a.go" || string(changes[0].Content) != "package a" || changes[0].Deleted {
		t.Fatalf("a.go change = %+v", changes[0])
	}
	if changes[1].Path != "gone.go" || !changes[1].Deleted || changes[1].Content != nil {
		t.Fatalf("gone.go change = %+v, want deleted with nil content", changes[1])
	}
}

func TestRefChangeSource_PropagatesReadError(t *testing.T) {
	changed := func(context.Context, string, string, string) ([]string, error) {
		return []string{"x.go"}, nil
	}
	at := func(context.Context, string, string, string) ([]byte, error) {
		return nil, fmt.Errorf("git object store unreadable")
	}
	src, _ := diffgate.NewRefChangeSource("/repo", "base", "cand", changed, at)
	if _, err := src.Changes(context.Background()); err == nil {
		t.Fatalf("expected a non-absence read error to propagate")
	}
}

func TestNewRefChangeSource_NilDependencies(t *testing.T) {
	ok := func(context.Context, string, string, string) ([]string, error) { return nil, nil }
	at := func(context.Context, string, string, string) ([]byte, error) { return nil, nil }
	cases := []struct {
		name             string
		root, base, cand string
		changed          diffgate.ChangedFilesBetweenFunc
		fileAt           diffgate.FileAtRefFunc
	}{
		{"no root", "", "b", "c", ok, at},
		{"no base", "/r", "", "c", ok, at},
		{"no cand", "/r", "b", "", ok, at},
		{"no changed", "/r", "b", "c", nil, at},
		{"no fileAt", "/r", "b", "c", ok, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := diffgate.NewRefChangeSource(tc.root, tc.base, tc.cand, tc.changed, tc.fileAt); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
