// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

func TestVacuumInto(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	db, err := sql.Open("sqlite3", srcPath)
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1), (2), (3)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := sqlite.VacuumInto(context.Background(), db, dstPath); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}

	if _, err := os.Stat(dstPath); err != nil {
		t.Fatalf("dest file not found: %v", err)
	}

	dst, err := sql.Open("sqlite3", dstPath)
	if err != nil {
		t.Fatalf("open dst db: %v", err)
	}
	defer dst.Close()

	var count int
	if err := dst.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows in snapshot, got %d", count)
	}
}

func TestVacuumInto_FailsIfDestExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "existing.db")

	db, err := sql.Open("sqlite3", srcPath)
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	defer db.Close()

	// Pre-create the destination file to ensure VacuumInto rejects it.
	if err := os.WriteFile(dstPath, []byte("existing"), 0o600); err != nil {
		t.Fatalf("write existing file: %v", err)
	}

	err = sqlite.VacuumInto(context.Background(), db, dstPath)
	if err == nil {
		t.Fatal("expected error when dest already exists, got nil")
		return
	}
}

func TestVacuumInto_ContextCancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	dstPath := filepath.Join(dir, "dst.db")

	db, err := sql.Open("sqlite3", srcPath)
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = sqlite.VacuumInto(ctx, db, dstPath)
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
		return
	}
}
