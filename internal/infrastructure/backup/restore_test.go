// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package backup_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
)

func makeBackup(t *testing.T) (tarPath, backupDir string) {
	t.Helper()
	srcHome := t.TempDir()
	backupDir = t.TempDir()
	dbPath := filepath.Join(srcHome, "veska.db")
	seedDB(t, dbPath)

	res, err := backup.Create(backup.CreateOptions{
		DBPath:    dbPath,
		VeskaHome: srcHome,
		BackupDir: backupDir,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return res.Path, backupDir
}

func TestRestoreRoundTrip(t *testing.T) {
	tarPath, _ := makeBackup(t)
	destHome := t.TempDir()

	res, err := backup.Restore(backup.RestoreOptions{
		TarballPath: tarPath,
		VeskaHome:   destHome,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	dbPath := filepath.Join(destHome, "veska.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}
	if res.DBSizeBytes <= 0 {
		t.Fatalf("RestoreResult.DBSizeBytes=%d, want >0", res.DBSizeBytes)
	}

	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id='n1'`).Scan(&name); err != nil {
		t.Fatalf("query restored db: %v", err)
	}
	if name != "hello" {
		t.Fatalf("restored row = %q, want hello", name)
	}
}

func TestRestoreRefusesCorruptTarball(t *testing.T) {
	tarPath, _ := makeBackup(t)
	if err := os.WriteFile(tarPath, []byte("not a gzip"), 0o600); err != nil {
		t.Fatalf("corrupt tarball: %v", err)
	}

	destHome := t.TempDir()
	existingDB := filepath.Join(destHome, "veska.db")
	seedDB(t, existingDB)
	before, err := os.ReadFile(existingDB)
	if err != nil {
		t.Fatalf("read existing db: %v", err)
	}

	_, err = backup.Restore(backup.RestoreOptions{
		TarballPath: tarPath,
		VeskaHome:   destHome,
	})
	if !errors.Is(err, backup.ErrBackupCorrupt) {
		t.Fatalf("Restore err = %v, want ErrBackupCorrupt", err)
	}

	after, err := os.ReadFile(existingDB)
	if err != nil {
		t.Fatalf("read existing db after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("existing veska.db was modified by a refused restore")
	}
}

func TestRestoreRescuesExistingDB(t *testing.T) {
	tarPath, _ := makeBackup(t)
	destHome := t.TempDir()
	existingDB := filepath.Join(destHome, "veska.db")
	seedDB(t, existingDB)

	res, err := backup.Restore(backup.RestoreOptions{
		TarballPath: tarPath,
		VeskaHome:   destHome,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.RescuePath == "" {
		t.Fatal("RestoreResult.RescuePath is empty; existing DB was not rescued")
	}
	if _, err := os.Stat(res.RescuePath); err != nil {
		t.Fatalf("rescue copy missing: %v", err)
	}
	base := filepath.Base(res.RescuePath)
	if !strings.HasPrefix(base, "veska.db.before-restore-") || !strings.HasSuffix(base, ".bak") {
		t.Fatalf("rescue copy name = %q, want veska.db.before-restore-<ts>.bak", base)
	}
}

func TestRestoreAbortsOnStaleRescueCopy(t *testing.T) {
	tarPath, _ := makeBackup(t)
	destHome := t.TempDir()
	seedDB(t, filepath.Join(destHome, "veska.db"))
	stale := filepath.Join(destHome, "veska.db.before-restore-20200101T000000Z.bak")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale rescue: %v", err)
	}

	_, err := backup.Restore(backup.RestoreOptions{
		TarballPath: tarPath,
		VeskaHome:   destHome,
	})
	if !errors.Is(err, backup.ErrStaleRescueCopy) {
		t.Fatalf("Restore err = %v, want ErrStaleRescueCopy", err)
	}
}

func TestSelectLatestPicksNewest(t *testing.T) {
	backupDir := t.TempDir()
	older := filepath.Join(backupDir, "veska-backup-20200101T000000Z.tar.gz")
	newer := filepath.Join(backupDir, "veska-backup-20250101T000000Z.tar.gz")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	got, err := backup.SelectLatest(backupDir)
	if err != nil {
		t.Fatalf("SelectLatest: %v", err)
	}
	if got != newer {
		t.Fatalf("SelectLatest = %q, want %q", got, newer)
	}
}

func TestSelectPreMigrationErrorsWhenNone(t *testing.T) {
	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "veska-backup-20250101T000000Z.tar.gz"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := backup.SelectPreMigration(backupDir)
	if !errors.Is(err, backup.ErrNoBackup) {
		t.Fatalf("SelectPreMigration err = %v, want ErrNoBackup", err)
	}
}

func TestPruneKeepsMinCount(t *testing.T) {
	backupDir := t.TempDir()
	old := time.Now().Add(-90 * 24 * time.Hour)
	var names []string
	for _, ts := range []string{
		"20200101T000000Z", "20200201T000000Z", "20200301T000000Z",
		"20200401T000000Z", "20200501T000000Z",
	} {
		n := filepath.Join(backupDir, "veska-backup-"+ts+".tar.gz")
		if err := os.WriteFile(n, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(n, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		names = append(names, n)
	}
	auto := filepath.Join(backupDir, "auto-pre-migration-1-to-2-20200101T000000Z.tar.gz")
	if err := os.WriteFile(auto, []byte("x"), 0o600); err != nil {
		t.Fatalf("write auto: %v", err)
	}
	if err := os.Chtimes(auto, old, old); err != nil {
		t.Fatalf("chtimes auto: %v", err)
	}

	res, err := backup.Prune(backup.PruneOptions{
		BackupDir:    backupDir,
		KeepMinCount: 3,
		MaxAge:       30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Kept != 3 {
		t.Fatalf("Prune kept %d, want 3", res.Kept)
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("Prune deleted %d, want 2", len(res.Deleted))
	}
	if _, err := os.Stat(auto); err != nil {
		t.Fatalf("auto-pre-migration snapshot was pruned: %v", err)
	}
	for _, n := range names[2:] {
		if _, err := os.Stat(n); err != nil {
			t.Fatalf("expected %s to survive: %v", n, err)
		}
	}
}

func TestPruneNoOpOnCleanDir(t *testing.T) {
	backupDir := t.TempDir()
	res, err := backup.Prune(backup.PruneOptions{
		BackupDir:    backupDir,
		KeepMinCount: 3,
		MaxAge:       30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune on clean dir: %v", err)
	}
	if len(res.Deleted) != 0 || res.Kept != 0 {
		t.Fatalf("Prune on clean dir: deleted=%d kept=%d, want 0/0", len(res.Deleted), res.Kept)
	}
}

func TestParseRetentionAge(t *testing.T) {
	cases := map[string]time.Duration{
		"30d":  30 * 24 * time.Hour,
		"720h": 720 * time.Hour,
		"":     0,
	}
	for in, want := range cases {
		got, err := backup.ParseRetentionAge(in)
		if err != nil {
			t.Fatalf("ParseRetentionAge(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseRetentionAge(%q) = %v, want %v", in, got, want)
		}
	}
}
