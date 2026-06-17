// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package backupcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/doctorcmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// RunCreate runs VACUUM INTO, copies supporting files, and writes a
// timestamped.tar.gz. outputDir == "" defaults to $VESKA_HOME/backups so a
// single `rm -rf $VESKA_HOME` clears all veska state.
func RunCreate(out io.Writer, outputDir string) error {
	veskaHome := config.DefaultVectorDir()
	if outputDir == "" {
		outputDir = config.DefaultBackupDir()
	}

	result, err := backup.Create(backup.CreateOptions{
		DBPath:    filepath.Join(veskaHome, "veska.db"),
		VeskaHome: veskaHome,
		BackupDir: outputDir,
	})
	if err != nil {
		return fmt.Errorf("backup create: %w", err)
	}

	fmt.Fprintf(out, "backup created: %s (%d bytes)\n", result.Path, result.SizeBytes)
	return nil
}

// RunPrune applies the retention policy: it keeps the
// [backup].keep_min_count most-recent user-initiated backups regardless of age
// and deletes the rest if older than [backup].keep_max_age. Auto-pre-migration
// snapshots are never pruned. Idempotent. backupDir == "" prunes the canonical
// $VESKA_HOME/backups; legacy ~/.veska-backups stays untouched unless named
// explicitly.
func RunPrune(out io.Writer, backupDir string) error {
	if backupDir == "" {
		backupDir = config.DefaultBackupDir()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("backup prune: load config: %w", err)
	}
	maxAge, err := backup.ParseRetentionAge(cfg.Backup.KeepMaxAge)
	if err != nil {
		return fmt.Errorf("backup prune: %w", err)
	}

	result, err := backup.Prune(backup.PruneOptions{
		BackupDir:    backupDir,
		KeepMinCount: cfg.Backup.KeepMinCount,
		MaxAge:       maxAge,
	})
	if err != nil {
		return fmt.Errorf("backup prune: %w", err)
	}

	fmt.Fprintf(out, "backup prune: kept %d, deleted %d\n", result.Kept, len(result.Deleted))
	for _, p := range result.Deleted {
		fmt.Fprintf(out, "  deleted %s\n", p)
	}
	return nil
}

// RunVerify extracts veska.db from the tarball, runs PRAGMA integrity_check and
// foreign_key_check, and validates audit.jsonl if present. A non-healthy status
// is returned as a doctorcmd.ProbeStatusError so the CLI exits with the
// code (0 healthy / 1 degraded / 2 broken).
func RunVerify(out io.Writer, tarPath string, jsonOut bool) error {
	tarPath = resolveTarballPath(tarPath)
	if _, err := os.Stat(tarPath); err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}

	result, err := backup.Verify(tarPath)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}

	if jsonOut {
		return json.NewEncoder(out).Encode(doctor.NewEnvelope("backup_verify", health.Status(result.Status), result))
	}

	fmt.Fprintf(out, "backup verify: %s (db_integrity=%v, foreign_key=%v, audit_present=%v, audit_ok=%v)\n",
		result.Status, result.DBIntegrityOK, result.ForeignKeyOK, result.AuditPresent, result.AuditJSONLOK)

	if result.Status != "healthy" {
		return doctorcmd.ProbeStatusError{Subsystem: "backup_verify", Status: result.Status}
	}
	return nil
}

// resolveTarballPath resolves a bare backup name (the NAME column from
// `backup list`) against the configured backups dir.: without this
// Verify's "couldn't open the archive" failure rendered as "broken
// (db_integrity=false,.)" - indistinguishable from a real corrupt-DB
// result, making fat-fingered paths look like healthy backups had gone bad.
func resolveTarballPath(tarPath string) string {
	if _, err := os.Stat(tarPath); err == nil {
		return tarPath
	}
	if filepath.IsAbs(tarPath) || strings.ContainsRune(tarPath, filepath.Separator) {
		return tarPath
	}
	candidate := filepath.Join(config.DefaultBackupDir(), tarPath)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return tarPath
}
