// SPDX-License-Identifier: AGPL-3.0-only

package sqldriver_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// TestConnectHookAppliesPerfPragmas verifies the registered driver runs the
// connection-scoped performance pragmas on a fresh connection - the path the
// runtime pools take, which never calls applyPragmas.
func TestConnectHookAppliesPerfPragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open(sqldriver.Name, sqldriver.BuildDSN(dbPath, 5000))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var cacheSize, tempStore int
	if err := db.QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil {
		t.Fatalf("cache_size: %v", err)
	}
	if cacheSize != -16000 {
		t.Errorf("cache_size = %d, want -16000", cacheSize)
	}
	if err := db.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil {
		t.Fatalf("temp_store: %v", err)
	}
	if tempStore != 2 { // 2 == MEMORY
		t.Errorf("temp_store = %d, want 2 (MEMORY)", tempStore)
	}

	// The existing DSN pragmas must still hold on the same connection.
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}
