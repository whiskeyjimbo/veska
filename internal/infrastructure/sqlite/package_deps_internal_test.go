// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"reflect"
	"testing"
)

func TestAggregatePackageDeps(t *testing.T) {
	const mod = "example.com/app"
	imports := []pkgImportRow{
		// a imports b and c (b appears twice across files -> deduped).
		{filePath: "a/a1.go", importPath: "example.com/app/b"},
		{filePath: "a/a2.go", importPath: "example.com/app/b"},
		{filePath: "a/a2.go", importPath: "example.com/app/c"},
		// b imports c.
		{filePath: "b/b.go", importPath: "example.com/app/c"},
		// self-import within the same package is dropped.
		{filePath: "c/c1.go", importPath: "example.com/app/c"},
		// a non-module path that slipped in is ignored (defensive).
		{filePath: "a/a1.go", importPath: "github.com/x/y"},
		// test-file imports are excluded: Go keeps them out of the build import
		// graph, so this c->a edge must NOT create an a<->c cycle.
		{filePath: "c/c_test.go", importPath: "example.com/app/a"},
	}
	got := aggregatePackageDeps(imports, mod)
	want := map[string][]string{
		"a": {"b", "c"},
		"b": {"c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aggregatePackageDeps:\n got %v\nwant %v", got, want)
	}
}

func TestPkgDir(t *testing.T) {
	cases := map[string]string{
		"internal/core/domain/edge.go": "internal/core/domain",
		"main.go":                      "",
		"cmd/veska/main.go":            "cmd/veska",
	}
	for in, want := range cases {
		if got := pkgDir(in); got != want {
			t.Errorf("pkgDir(%q) = %q, want %q", in, got, want)
		}
	}
}
