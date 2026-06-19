// SPDX-License-Identifier: AGPL-3.0-only

package fatfiles_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/lint/fatfiles"
)

// TestCheck covers the three ratchet behaviors from the acceptance criteria.
func TestCheck(t *testing.T) {
	t.Parallel()

	inv := []fatfiles.Entry{
		{Path: "a.go", MaxLOC: 100},
		{Path: "b.go", MaxLOC: 200},
	}

	t.Run("grown file is a violation", func(t *testing.T) {
		t.Parallel()
		v := fatfiles.Check(inv, map[string]int{"a.go": 101, "b.go": 200})
		if len(v) != 1 {
			t.Fatalf("want 1 violation, got %d: %v", len(v), v)
		}
		if v[0].Path != "a.go" || v[0].Missing {
			t.Errorf("want grow violation on a.go, got %+v", v[0])
		}
	})

	t.Run("all at recorded size is clean", func(t *testing.T) {
		t.Parallel()
		v := fatfiles.Check(inv, map[string]int{"a.go": 100, "b.go": 200})
		if len(v) != 0 {
			t.Errorf("want no violations at baseline, got %v", v)
		}
	})

	t.Run("shrunk file with lowered record is clean (ratchet down)", func(t *testing.T) {
		t.Parallel()
		lowered := []fatfiles.Entry{{Path: "a.go", MaxLOC: 90}, {Path: "b.go", MaxLOC: 200}}
		v := fatfiles.Check(lowered, map[string]int{"a.go": 90, "b.go": 200})
		if len(v) != 0 {
			t.Errorf("want no violations after ratchet-down, got %v", v)
		}
	})

	t.Run("file below ceiling is clean even without lowering", func(t *testing.T) {
		t.Parallel()
		v := fatfiles.Check(inv, map[string]int{"a.go": 50, "b.go": 200})
		if len(v) != 0 {
			t.Errorf("under-ceiling files must not violate, got %v", v)
		}
	})

	t.Run("inventoried-but-missing file is a stale violation", func(t *testing.T) {
		t.Parallel()
		v := fatfiles.Check(inv, map[string]int{"b.go": 200})
		if len(v) != 1 || !v[0].Missing || v[0].Path != "a.go" {
			t.Errorf("want stale violation on a.go, got %v", v)
		}
	})
}

func TestParseInventory(t *testing.T) {
	t.Parallel()
	in := "# comment\n\ncmd/veska/repo.go 1221\n  internal/x.go   42  \n"
	got, err := fatfiles.ParseInventory(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []fatfiles.Entry{
		{Path: "cmd/veska/repo.go", MaxLOC: 1221},
		{Path: "internal/x.go", MaxLOC: 42},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestParseInventoryBadLine(t *testing.T) {
	t.Parallel()
	if _, err := fatfiles.ParseInventory(strings.NewReader("only-one-field\n")); err == nil {
		t.Error("want error on malformed line")
	}
	if _, err := fatfiles.ParseInventory(strings.NewReader("path notanumber\n")); err == nil {
		t.Error("want error on non-numeric LOC")
	}
}
