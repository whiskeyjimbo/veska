// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// TestPackageDeps_InternalImportsPersistedAndAggregated promotes a file with an
// internal import, an external module import, and a stdlib import, then asserts:
// the internal/external flag is set correctly, `deps list` (ListImports) stays
// external-only, and PackageDependencies aggregates the internal edge.
func TestPackageDeps_InternalImportsPersistedAndAggregated(t *testing.T) {
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "example.com/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})

	// a imports b (internal), an external module, and stdlib. Unused imports are
	// fine: the tree-sitter parser extracts them without type-checking.
	srcA := `package a

import (
	"fmt"
	"example.com/app/b"
	"github.com/x/y"
)

type A struct{}
`
	srcB := `package b

type B struct{}
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files: []application.PromotionFile{
			parseToFile(t, "repo1", "a/a.go", srcA),
			parseToFile(t, "repo1", "b/b.go", srcB),
		},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}

	// Internal import flagged internal=1; external flagged internal=0; stdlib absent.
	if n := count(`SELECT COUNT(*) FROM file_imports WHERE import_path='example.com/app/b' AND internal=1`); n != 1 {
		t.Errorf("internal import b: got %d rows with internal=1, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM file_imports WHERE import_path='github.com/x/y' AND internal=0`); n != 1 {
		t.Errorf("external import y: got %d rows with internal=0, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM file_imports WHERE import_path='fmt'`); n != 0 {
		t.Errorf("stdlib import fmt: got %d rows, want 0 (skipped)", n)
	}

	// deps list stays external-only.
	deps := sqlite.NewDependenciesRepo(db)
	imps, err := deps.ListImports(context.Background(), "repo1", "main")
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	for _, i := range imps {
		if i.ImportPath == "example.com/app/b" {
			t.Errorf("ListImports leaked an internal import: %q", i.ImportPath)
		}
	}
	if len(imps) != 1 || imps[0].ImportPath != "github.com/x/y" {
		t.Errorf("ListImports: got %v, want only [github.com/x/y]", imps)
	}

	// Package dependency aggregation: a -> b.
	pkgDeps := sqlite.NewPackageDepsRepo(db)
	graph, err := pkgDeps.PackageDependencies(context.Background(), "repo1", "main")
	if err != nil {
		t.Fatalf("PackageDependencies: %v", err)
	}
	if got := graph["a"]; len(got) != 1 || got[0] != "b" {
		t.Errorf("package deps for a: got %v, want [b]", got)
	}
	if _, ok := graph["b"]; ok {
		t.Errorf("package b should have no outgoing internal deps, got %v", graph["b"])
	}
}
