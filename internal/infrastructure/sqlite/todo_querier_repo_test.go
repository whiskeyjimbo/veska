package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func openTodoTestDB(t *testing.T) *sqlite.TodoQuerierRepo {
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

	insert := func(id, branch, rule, file, state string) {
		if _, err := db.Exec(`INSERT INTO findings
			(finding_id, branch, repo_id, file_path, severity, source_layer, rule, message, state, created_at, actor_id, actor_kind)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, branch, "r1", file, "info", "structural", rule, "msg", state, now, "service:veska", "system"); err != nil {
			t.Fatalf("insert finding: %v", err)
		}
	}
	insert("t1", "main", "todo", "a.go", "open")
	insert("t2", "main", "todo", "b.go", "open")
	insert("t3", "main", "todo", "c.go", "closed")
	insert("t4", "main", "parse-failure", "d.go", "open") // different rule
	insert("t5", "other", "todo", "a.go", "open")         // different branch

	return sqlite.NewTodoQuerierRepo(db)
}

func TestFindTodos_OnlyOpenFiltersClosedAndOtherRules(t *testing.T) {
	r := openTodoTestDB(t)
	got, err := r.FindTodos(context.Background(), "r1", "main", true)
	if err != nil {
		t.Fatalf("FindTodos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 open todos on main, got %d (%+v)", len(got), got)
	}
	for _, e := range got {
		if e.State != "open" {
			t.Errorf("found non-open: %+v", e)
		}
		if e.Branch != "main" {
			t.Errorf("found cross-branch: %+v", e)
		}
	}
}

func TestFindTodos_IncludesClosedWhenOnlyOpenFalse(t *testing.T) {
	r := openTodoTestDB(t)
	got, err := r.FindTodos(context.Background(), "r1", "main", false)
	if err != nil {
		t.Fatalf("FindTodos: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 (open + closed) todos on main, got %d", len(got))
	}
}

func TestFindTodos_UnknownRepoReturnsEmpty(t *testing.T) {
	r := openTodoTestDB(t)
	got, err := r.FindTodos(context.Background(), "ghost", "main", true)
	if err != nil {
		t.Fatalf("FindTodos: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}
