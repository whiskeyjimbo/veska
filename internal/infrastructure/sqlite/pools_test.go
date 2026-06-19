// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

const memDSN = "file::memory:?mode=memory&cache=shared"

// TestOpenPools_ForeignKeyCascade verifies that ON DELETE CASCADE correctly fires
// across pooled connections when foreign key enforcement is DSN-scoped.
func TestOpenPools_ForeignKeyCascade(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "v.db")

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
	if _, err := pools.Write.Exec(`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`, "r1", "/tmp/r1", now); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := pools.Write.Exec(`INSERT INTO nodes
		(node_id, branch, repo_id, language, kind, symbol_path, file_path, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"n1", "main", "r1", "go", "function", "Foo", "foo.go", "h", now, "service:veska", "system"); err != nil {
		t.Fatalf("insert node: %v", err)
	}

	if _, err := pools.Write.Exec(`DELETE FROM repos WHERE repo_id = ?`, "r1"); err != nil {
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

func TestOpenPools_ReturnsBothHandles(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	if pools.ReadDB == nil {
		t.Error("ReadDB is nil")
	}
	if pools.Write == nil {
		t.Error("Write is nil")
	}
}

func TestOpenPools_ReadDB_UnlimitedConnections(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	if got := pools.ReadDB.Stats().MaxOpenConnections; got != 0 {
		t.Errorf("ReadDB.MaxOpenConnections = %d; want 0 (unlimited)", got)
	}
}

func TestOpenPools_WriteHandle_MaxOneConn(t *testing.T) {
	t.Parallel()

	pools, err := sqlite.OpenPools(memDSN)
	if err != nil {
		t.Fatalf("OpenPools: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	if got := pools.Write.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("Write.MaxOpenConnections = %d; want 1", got)
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
		{"Write", pools.Write},
	}
	for _, h := range handles {
		var mode string
		if err := h.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Errorf("%s: PRAGMA journal_mode: %v", h.name, err)
			continue
		}
		// In-memory SQLite databases always return "memory" for journal_mode because
		// WAL is only supported by file-backed databases.
		if mode != "wal" && mode != "memory" {
			t.Errorf("%s: journal_mode = %q; want \"wal\" or \"memory\"", h.name, mode)
		}
	}
}

// TestOpenPools_ConcurrentWrites_NoSQLITEBUSY verifies that forcing a single
// connection on the write pool prevents concurrent transactions from failing
// with SQLITE_BUSY errors.
func TestOpenPools_ConcurrentWrites_NoSQLITEBUSY(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "v.db")

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

	const writers = 8
	const txnsPerWriter = 25

	var wg sync.WaitGroup
	errs := make(chan error, writers*txnsPerWriter)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range txnsPerWriter {
				tx, beginErr := pools.Write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
				if beginErr != nil {
					errs <- beginErr
					return
				}
				repoID := "r-" + string(rune('a'+id)) + "-" + string(rune('a'+i%26))
				if _, execErr := tx.ExecContext(ctx,
					`INSERT INTO repos (repo_id, root_path, added_at) VALUES (?,?,?)`,
					repoID, "/tmp/"+repoID, now,
				); execErr != nil {
					_ = tx.Rollback()
					errs <- execErr
					return
				}
				if _, execErr := tx.ExecContext(ctx,
					`DELETE FROM repos WHERE repo_id = ?`, repoID,
				); execErr != nil {
					_ = tx.Rollback()
					errs <- execErr
					return
				}
				if commitErr := tx.Commit(); commitErr != nil {
					errs <- commitErr
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil && (errors.Is(err, sql.ErrTxDone) || strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked")) {
			t.Errorf("concurrent writer hit SQLITE_BUSY/locked: %v", err)
		} else if err != nil {
			t.Errorf("concurrent writer unexpected error: %v", err)
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
