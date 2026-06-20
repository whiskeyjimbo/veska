// SPDX-License-Identifier: AGPL-3.0-only

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
)

func TestTouchEphemeral_UpdatesOnlyEphemeralRows(t *testing.T) {
	db := newTestDB(t)

	// Seed one tracked and one ephemeral repository record.
	for _, args := range [][]any{
		{"tracked-id", "/tmp/tracked", int64(1000), "tracked"},
		{"ephemeral-id", "/tmp/ephemeral", int64(1000), "ephemeral"},
	} {
		if _, err := db.Exec(
			`INSERT INTO repos (repo_id, root_path, added_at, kind) VALUES (?, ?, ?, ?)`,
			args...,
		); err != nil {
			t.Fatalf("seed %v: %v", args, err)
		}
	}

	if err := repo.TouchEphemeral(context.Background(), db, "tracked-id"); err != nil {
		t.Fatalf("TouchEphemeral(tracked): %v", err)
	}
	if err := repo.TouchEphemeral(context.Background(), db, "ephemeral-id"); err != nil {
		t.Fatalf("TouchEphemeral(ephemeral): %v", err)
	}

	var trackedAccessed, ephemeralAccessed sql.NullInt64
	if err := db.QueryRow(`SELECT last_accessed_at FROM repos WHERE repo_id = ?`, "tracked-id").Scan(&trackedAccessed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT last_accessed_at FROM repos WHERE repo_id = ?`, "ephemeral-id").Scan(&ephemeralAccessed); err != nil {
		t.Fatal(err)
	}

	if trackedAccessed.Valid {
		t.Errorf("tracked row's last_accessed_at = %d, want NULL (must not be touched)", trackedAccessed.Int64)
	}
	if !ephemeralAccessed.Valid || ephemeralAccessed.Int64 == 0 {
		t.Errorf("ephemeral row's last_accessed_at = %v, want a positive unix timestamp", ephemeralAccessed)
	}
}

func TestTouchEphemeral_MissingRowIsNoError(t *testing.T) {
	db := newTestDB(t)

	if err := repo.TouchEphemeral(context.Background(), db, "no-such-id"); err != nil {
		t.Errorf("TouchEphemeral on missing row: %v (want nil)", err)
	}
}
