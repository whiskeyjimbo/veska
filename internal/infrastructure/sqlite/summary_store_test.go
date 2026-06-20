// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/summary"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func TestSummaryStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	ctx := context.Background()

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES ('r1','/tmp/r1',0)`,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Two nodes in the same file: a function (summarizable) and a package
	// (container) — the store returns both; kind filtering is the lane's job.
	_, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES
		('n_fn','main','r1','go','function','Add','math.go',3,5,'h1',0,'a','system'),
		('n_pkg','main','r1','go','package','mathutil','math.go',1,1,'h2',0,'a','system')`)
	if err != nil {
		t.Fatalf("insert nodes: %v", err)
	}

	store := sqlite.NewSummaryStore(db, db)

	got, err := store.PromotedNodes(ctx, "r1", "main", "math.go")
	if err != nil {
		t.Fatalf("PromotedNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PromotedNodes returned %d nodes, want 2", len(got))
	}
	var fn summary.Node
	for _, n := range got {
		if n.NodeID == "n_fn" {
			fn = n
		}
	}
	if fn.Name != "Add" || fn.Kind != "function" || fn.LineStart != 3 || fn.LineEnd != 5 {
		t.Fatalf("function node projection wrong: %+v", fn)
	}

	// Write a summary and read it back via the column.
	if err := store.SetShortSummary(ctx, "r1", "main", "n_fn", "adds two integers"); err != nil {
		t.Fatalf("SetShortSummary: %v", err)
	}
	var stored *string
	if err := db.QueryRow(
		`SELECT short_summary FROM nodes WHERE node_id='n_fn' AND branch='main' AND repo_id='r1'`,
	).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored == nil || *stored != "adds two integers" {
		t.Fatalf("stored short_summary = %v, want %q", stored, "adds two integers")
	}

	// The untouched node stays NULL (heuristic fallback territory).
	var pkgSummary *string
	if err := db.QueryRow(
		`SELECT short_summary FROM nodes WHERE node_id='n_pkg'`,
	).Scan(&pkgSummary); err != nil {
		t.Fatalf("read pkg: %v", err)
	}
	if pkgSummary != nil {
		t.Fatalf("untouched node short_summary = %q, want NULL", *pkgSummary)
	}
}

// TestSummaryStore_HydratesIntoNodeRead closes the read loop: a summary written
// by the lane must rehydrate into domain.Node.ShortSummary via the shared
// scanNode path (FindNodeByID), which is what feeds the MCP node projection.
func TestSummaryStore_HydratesIntoNodeRead(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	ctx := context.Background()

	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES ('r1','/tmp/r1',0)`,
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (
		node_id, branch, repo_id, language, kind, symbol_path, file_path,
		line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
	) VALUES ('n_fn','main','r1','go','function','Add','math.go',3,5,'h1',0,'a','system')`); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	store := sqlite.NewSummaryStore(db, db)
	if err := store.SetShortSummary(ctx, "r1", "main", "n_fn", "adds two integers"); err != nil {
		t.Fatalf("SetShortSummary: %v", err)
	}

	repo := sqlite.NewGraphRepo(db, db)
	n, err := repo.FindNodeByID(ctx, "n_fn")
	if err != nil {
		t.Fatalf("FindNodeByID: %v", err)
	}
	if n == nil || n.ShortSummary == nil {
		t.Fatalf("ShortSummary did not hydrate: %+v", n)
	}
	if *n.ShortSummary != "adds two integers" {
		t.Fatalf("hydrated summary = %q, want %q", *n.ShortSummary, "adds two integers")
	}
}

func TestSummaryStore_SetMissingNodeIsNoError(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))
	store := sqlite.NewSummaryStore(db, db)
	// Updating a non-existent node affects zero rows; a concurrent reparse can
	// delete a node between load and write, so this must not error.
	if err := store.SetShortSummary(context.Background(), "r1", "main", "ghost", "x"); err != nil {
		t.Fatalf("SetShortSummary on missing node: %v", err)
	}
}
