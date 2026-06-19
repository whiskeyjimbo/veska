// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// contractDriftFixture maintains test database state for ContractDriftRepo tests.
type contractDriftFixture struct {
	db     *sql.DB
	repoID string
	branch string
}

func setupContractDriftFixture(t *testing.T) *contractDriftFixture {
	t.Helper()
	dir := t.TempDir()
	db := openTest(t, filepath.Join(dir, "v.db"))

	repoID := "repo1"
	branch := "main"
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?, ?, ?)`,
		repoID, "/tmp/repo1", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	return &contractDriftFixture{db: db, repoID: repoID, branch: branch}
}

// insertNode seeds a node row with explicit signature values, treating empty strings as NULL.
func (f *contractDriftFixture) insertNode(t *testing.T, nodeID, filePath, kind, name, prevSig, sig string) {
	t.Helper()
	var prevArg, sigArg any
	if prevSig == "" {
		prevArg = nil
	} else {
		prevArg = prevSig
	}
	if sig == "" {
		sigArg = nil
	} else {
		sigArg = sig
	}
	_, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
        signature, prev_signature
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nodeID, f.branch, f.repoID, "go", kind, name, filePath,
		1, 10, "h-"+nodeID, time.Now().UnixMilli(), "service:veska", "system",
		sigArg, prevArg,
	)
	if err != nil {
		t.Fatalf("insert node %s: %v", nodeID, err)
	}
}

func TestContractDriftRepo_FlagsDriftedNodesOnly(t *testing.T) {
	t.Parallel()
	f := setupContractDriftFixture(t)

	f.insertNode(t, "n-drift", "pkg/a.go", "function", "Foo", "old", "new")
	f.insertNode(t, "n-stable", "pkg/a.go", "function", "Bar", "same", "same")
	f.insertNode(t, "n-new", "pkg/a.go", "function", "Baz", "", "fresh")
	f.insertNode(t, "n-type", "pkg/a.go", "type", "T", "old", "new")
	f.insertNode(t, "n-oos", "pkg/elsewhere.go", "function", "X", "old", "new")

	repo := sqlite.NewContractDriftRepo(f.db)
	got, err := repo.DriftedNodesInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("DriftedNodesInFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d drifted nodes, want 1: %+v", len(got), got)
	}
	if got[0].NodeID != "n-drift" {
		t.Errorf("wrong node returned: %+v", got[0])
	}
	if got[0].PrevSig != "old" || got[0].NewSig != "new" {
		t.Errorf("sig fields wrong: prev=%q new=%q", got[0].PrevSig, got[0].NewSig)
	}
	if got[0].Kind != "function" {
		t.Errorf("kind: %q", got[0].Kind)
	}
}

func TestContractDriftRepo_AcceptsMethodAndInterfaceKinds(t *testing.T) {
	t.Parallel()
	f := setupContractDriftFixture(t)

	f.insertNode(t, "n-fn", "pkg/a.go", "function", "F", "a", "b")
	f.insertNode(t, "n-m", "pkg/a.go", "method", "M", "a", "b")
	f.insertNode(t, "n-i", "pkg/a.go", "interface", "I", "a", "b")
	f.insertNode(t, "n-c", "pkg/a.go", "class", "C", "a", "b")
	f.insertNode(t, "n-s", "pkg/a.go", "struct", "S", "a", "b")

	repo := sqlite.NewContractDriftRepo(f.db)
	got, err := repo.DriftedNodesInFiles(context.Background(), f.repoID, f.branch, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("DriftedNodesInFiles: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3: %+v", len(got), got)
	}
	ids := []string{got[0].NodeID, got[1].NodeID, got[2].NodeID}
	sort.Strings(ids)
	want := []string{"n-fn", "n-i", "n-m"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d]=%q, want %q", i, ids[i], want[i])
		}
	}
}

func TestContractDriftRepo_EmptyFilePathsIsNoOp(t *testing.T) {
	t.Parallel()
	f := setupContractDriftFixture(t)
	f.insertNode(t, "n-drift", "pkg/a.go", "function", "Foo", "old", "new")

	repo := sqlite.NewContractDriftRepo(f.db)
	got, err := repo.DriftedNodesInFiles(context.Background(), f.repoID, f.branch, nil)
	if err != nil {
		t.Fatalf("DriftedNodesInFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 rows for empty paths, got %d", len(got))
	}
}

func TestContractDriftRepo_ScopesByRepoAndBranch(t *testing.T) {
	t.Parallel()
	f := setupContractDriftFixture(t)

	f.insertNode(t, "n-main", "pkg/a.go", "function", "M", "a", "b")
	// A separate branch row for the same node ID is allowed since the primary key spans both node ID and branch.
	if _, err := f.db.Exec(`INSERT INTO nodes (
        node_id, branch, repo_id, language, kind, symbol_path, file_path,
        line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind,
        signature, prev_signature
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"n-main", "feat/x", f.repoID, "go", "function", "M", "pkg/a.go",
		1, 10, "h-feat", time.Now().UnixMilli(), "service:veska", "system",
		"q", "q",
	); err != nil {
		t.Fatalf("insert feat node: %v", err)
	}

	repo := sqlite.NewContractDriftRepo(f.db)

	got, err := repo.DriftedNodesInFiles(context.Background(), f.repoID, "main", []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("query main: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("main: want 1, got %d", len(got))
	}

	got, err = repo.DriftedNodesInFiles(context.Background(), f.repoID, "feat/x", []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("query feat/x: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("feat/x: want 0, got %d", len(got))
	}
}
