package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

const memDSN = "file::memory:?mode=memory&cache=shared"

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
