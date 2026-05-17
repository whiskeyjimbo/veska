package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func openFindingQuerierTestDB(t *testing.T) *sqlite.FindingQuerierRepo {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.OpenWithOptions(filepath.Join(dir, "veska.db"), sqlite.Options{
		BackupDir: filepath.Join(t.TempDir(), "bk"),
	})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	insert := func(id, branch string, nodeID any, state string) {
		if _, err := db.Exec(`INSERT INTO findings
			(finding_id, branch, repo_id, node_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, branch, "r1", nodeID, "a.go", "info", "structural", "rule", "msg", state, now, "service:veska", "system"); err != nil {
			t.Fatalf("insert finding: %v", err)
		}
	}
	insert("f1", "main", "n1", "open")
	insert("f2", "main", "n2", "open")
	insert("f3", "main", "n3", "closed") // closed -> excluded
	insert("f4", "main", nil, "open")    // NULL node_id -> excluded
	insert("f5", "other", "n9", "open")  // different branch -> excluded
	insert("f6", "main", "n1", "open")   // duplicate node -> still one entry

	return sqlite.NewFindingQuerierRepo(db)
}

func TestOpenFindingNodeIDs_ReturnsOnlyOpenNodeScopedSet(t *testing.T) {
	r := openFindingQuerierTestDB(t)
	got, err := r.OpenFindingNodeIDs(context.Background(), "r1", "main")
	if err != nil {
		t.Fatalf("OpenFindingNodeIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct open-finding nodes, got %d (%+v)", len(got), got)
	}
	if !got["n1"] || !got["n2"] {
		t.Errorf("expected n1 and n2 in set, got %+v", got)
	}
	if got["n3"] {
		t.Errorf("closed finding's node leaked into set")
	}
	if got["n9"] {
		t.Errorf("cross-branch finding's node leaked into set")
	}
}

func TestOpenFindingNodeIDs_UnknownRepoReturnsEmpty(t *testing.T) {
	r := openFindingQuerierTestDB(t)
	got, err := r.OpenFindingNodeIDs(context.Background(), "ghost", "main")
	if err != nil {
		t.Fatalf("OpenFindingNodeIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty set, got %+v", got)
	}
}
