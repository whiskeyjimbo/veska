package sqlite_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// seedEdgesForReader inserts three nodes and two CALLS edges forming a→b→c for testing.
func seedEdgesForReader(t *testing.T) (*sqlite.EdgeReaderRepo, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "veska.db")
	backupDir := filepath.Join(t.TempDir(), "backups")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenWithOptions: %v", err)
	}
	cleanup := func() { _ = db.Close() }

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
		"r1", "/tmp/r1", now); err != nil {
		cleanup()
		t.Fatalf("insert repo: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.Exec(`INSERT INTO nodes (
			node_id, branch, repo_id, language, kind, symbol_path, file_path,
			line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "main", "r1", "go", "function", id, id+".go",
			1, 10, "h-"+id, now, "service:veska", "system"); err != nil {
			cleanup()
			t.Fatalf("insert node %s: %v", id, err)
		}
	}
	er := sqlite.NewEdgeRepo(db)
	ab, _ := domain.NewEdge(domain.EdgeSpec{Src: "a", Tgt: "b", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
	bc, _ := domain.NewEdge(domain.EdgeSpec{Src: "b", Tgt: "c", Kind: domain.EdgeCalls}, domain.WithConfidence(domain.Definite))
	if err := er.SaveEdges(context.Background(), "r1", "main", []*domain.Edge{ab, bc}); err != nil {
		cleanup()
		t.Fatalf("SaveEdges: %v", err)
	}
	return sqlite.NewEdgeReaderRepo(db), cleanup
}

func TestEdgeReaderRepo_InboundEdges(t *testing.T) {
	r, cleanup := seedEdgesForReader(t)
	defer cleanup()

	got, err := r.InboundEdges(context.Background(), "r1", "main", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("InboundEdges: %v", err)
	}
	if len(got["a"]) != 0 {
		t.Errorf("a has no callers, got %v", got["a"])
	}
	if !equalSorted(got["b"], []string{"a"}) {
		t.Errorf("b inbound: got %v want [a]", got["b"])
	}
	if !equalSorted(got["c"], []string{"b"}) {
		t.Errorf("c inbound: got %v want [b]", got["c"])
	}
	if _, ok := got["a"]; !ok {
		t.Error("expected key 'a' to be present in result map")
	}
}

func TestEdgeReaderRepo_OutboundEdges(t *testing.T) {
	r, cleanup := seedEdgesForReader(t)
	defer cleanup()

	got, err := r.OutboundEdges(context.Background(), "r1", "main", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("OutboundEdges: %v", err)
	}
	if !equalSorted(got["a"], []string{"b"}) {
		t.Errorf("a outbound: got %v want [b]", got["a"])
	}
	if !equalSorted(got["b"], []string{"c"}) {
		t.Errorf("b outbound: got %v want [c]", got["b"])
	}
	if len(got["c"]) != 0 {
		t.Errorf("c outbound should be empty, got %v", got["c"])
	}
}

func TestEdgeReaderRepo_EmptyInputShortCircuits(t *testing.T) {
	r, cleanup := seedEdgesForReader(t)
	defer cleanup()

	got, err := r.InboundEdges(context.Background(), "r1", "main", nil)
	if err != nil {
		t.Fatalf("InboundEdges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty input should yield empty map, got %v", got)
	}
}

func TestEdgeReaderRepo_ScopedByRepoAndBranch(t *testing.T) {
	r, cleanup := seedEdgesForReader(t)
	defer cleanup()

	got, err := r.InboundEdges(context.Background(), "r1", "other-branch", []string{"b"})
	if err != nil {
		t.Fatalf("InboundEdges: %v", err)
	}
	if len(got["b"]) != 0 {
		t.Errorf("other-branch should have no rows, got %v", got["b"])
	}
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}
