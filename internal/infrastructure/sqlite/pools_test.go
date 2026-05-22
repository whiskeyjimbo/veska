package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

const memDSN = "file::memory:?mode=memory&cache=shared"

// TestOpenPools_ForeignKeyCascade pins solov2-d78r: deleting a repos row must
// cascade-delete its child rows. This only works if foreign_keys is ON for the
// connection running the DELETE — and the pool opens many connections, so the
// pragma must be in the DSN (applied per-connection), not a one-shot Exec.
func TestOpenPools_ForeignKeyCascade(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "v.db")

	// Create + migrate the schema, then close that handle.
	mdb, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	_ = mdb.Close()

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	now := time.Now().UnixMilli()
	if _, err := pools.WriteHot.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, "r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := pools.WriteHot.Exec(`INSERT INTO nodes
		(node_id, branch, repo_id, language, kind, symbol_path, file_path, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n1", "main", "r1", "go", "function", "Foo", "foo.go", "h", now, "service:veska", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	if _, err := pools.WriteHot.Exec(`DELETE FROM repos WHERE repo_id = ?`, "r1"); err != nil {
		t.Fatalf("delete repo: %v", err)
	}

	var n int
	if err := pools.ReadDB.QueryRow(`SELECT COUNT(*) FROM nodes WHERE repo_id = ?`, "r1").Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if n != 0 {
		t.Errorf("ON DELETE CASCADE did not fire: %d node rows orphaned after repo delete", n)
	}
}

func TestOpenPools_ReturnsThreeHandles(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	if pools.ReadDB == nil {
		t.Error("ReadDB is nil")
	}
	if pools.WriteHot == nil {
		t.Error("WriteHot is nil")
	}
	if pools.WriteEmbed == nil {
		t.Error("WriteEmbed is nil")
	}
}

func TestOpenPools_ReadDB_UnlimitedConnections(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	// MaxOpenConnections == 0 means unlimited.
	if got := pools.ReadDB.Stats().MaxOpenConnections; got != 0 {
		t.Errorf("ReadDB.MaxOpenConnections = %d; want 0 (unlimited)", got)
	}
}

func TestOpenPools_WriteHandles_MaxOneConn(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	if got := pools.WriteHot.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("WriteHot.MaxOpenConnections = %d; want 1", got)
	}
	if got := pools.WriteEmbed.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("WriteEmbed.MaxOpenConnections = %d; want 1", got)
	}
}

func TestOpenPools_WALMode(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	handles := []struct {
		name string
		db   *sql.DB
	}{
		{"ReadDB", pools.ReadDB},
		{"WriteHot", pools.WriteHot},
		{"WriteEmbed", pools.WriteEmbed},
	}
	for _, h := range handles {
		var mode string
		if err := h.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Errorf("%s: PRAGMA journal_mode: %v", h.name, err)
			continue
		}
		// In-memory SQLite always returns "memory" for journal_mode; WAL is only
		// available for file-backed databases.  Accept both.
		if mode != "wal" && mode != "memory" {
			t.Errorf("%s: journal_mode = %q; want \"wal\" or \"memory\"", h.name, mode)
		}
	}
}

func TestOpenPools_Close_NoError(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	if err := pools.Close(); err != nil {
		t.Errorf("Close: unexpected error: %v", err)
	}
}
