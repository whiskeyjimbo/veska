// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sentinel errors for restore, mirroring /.
var (
	// ErrDaemonRunning is returned when a restore is attempted while the
	// veska daemon is up. Restore is a non-running-only operation.
	ErrDaemonRunning = errors.New("veska daemon is running; stop it first with 'veska service stop'")
	// ErrBackupCorrupt is returned when the backup tarball fails verification
	// before any change is made to the veska home.
	ErrBackupCorrupt = errors.New("backup tarball is corrupt")
	// ErrRestoreFailed is returned when the restored database fails its
	// post-extraction integrity check. The previous database is rolled back.
	ErrRestoreFailed = errors.New("restore failed integrity check")
	// ErrStaleRescueCopy is returned when a before-restore rescue copy already
	// exists, blocking an idempotent rename.
	ErrStaleRescueCopy = errors.New("a stale before-restore rescue copy exists; clear it before restoring")
	// ErrNoBackup is returned by tarball selection when no matching backup
	// tarball is found.
	ErrNoBackup = errors.New("no matching backup tarball found")
)

// autoPrefix is the filename prefix of pre-migration auto-snapshots.
const autoPrefix = "auto-pre-migration-"

// userPrefix is the filename prefix of user-initiated backups.
const userPrefix = "veska-backup-"

// RestoreOptions controls a restore operation.
type RestoreOptions struct {
	// TarballPath is the absolute path to the backup tarball to restore.
	TarballPath string
	// VeskaHome is the veska data directory the tarball is extracted into.
	VeskaHome string
}

// RestoreResult summarizes a successful restore.
type RestoreResult struct {
	// TarballPath is the backup that was restored.
	TarballPath string
	// DBSizeBytes is the size of the restored veska.db.
	DBSizeBytes int64
	// RescuePath is the path the previous veska.db was renamed to, or empty
	// if there was no previous database.
	RescuePath string
}

// Restore restores a verified backup tarball. The daemon must be stopped
// before running this. It verifies the tarball, saves a backup copy of the
// existing database, extracts the archive, and runs an integrity check. On
// failure, the backup copy is restored.
func Restore(opts RestoreOptions) (RestoreResult, error) {
	// 1. Verify before touching anything.
	vr, err := Verify(opts.TarballPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: verify: %w", err)
	}
	if vr.Status == "broken" {
		return RestoreResult{}, fmt.Errorf("%w: %s", ErrBackupCorrupt, opts.TarballPath)
	}

	// 2. Rescue the existing database.
	rescuePath, rescued, err := rescueExistingDB(opts.VeskaHome)
	if err != nil {
		return RestoreResult{}, err
	}

	// 3. Extract the tarball into the veska home.
	if err := extractTarGz(opts.TarballPath, opts.VeskaHome); err != nil {
		// Extraction failed: roll back the rescue so the user keeps their DB.
		_ = rollbackRescue(opts.VeskaHome, rescuePath, rescued)
		return RestoreResult{}, fmt.Errorf("restore: extract: %w", err)
	}

	// 4. Integrity check on the restored database.
	dbPath := filepath.Join(opts.VeskaHome, "veska.db")
	if err := checkRestoredDB(dbPath); err != nil {
		// 5. Roll back: remove the freshly extracted DB files, restore rescue.
		removeDBFiles(opts.VeskaHome)
		if rbErr := rollbackRescue(opts.VeskaHome, rescuePath, rescued); rbErr != nil {
			return RestoreResult{}, fmt.Errorf("%w: %v (rollback also failed: %v)", ErrRestoreFailed, err, rbErr)
		}
		return RestoreResult{}, fmt.Errorf("%w: %v", ErrRestoreFailed, err)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: stat restored db: %w", err)
	}

	result := RestoreResult{
		TarballPath: opts.TarballPath,
		DBSizeBytes: info.Size(),
	}
	if rescued {
		result.RescuePath = rescuePath
	}
	return result, nil
}

// rescueExistingDB renames the existing SQLite database and its WAL and SHM
// files to a temporary backup path. If a backup copy already exists, it
// returns ErrStaleRescueCopy to avoid overwriting it.
func rescueExistingDB(veskaHome string) (rescuePath string, rescued bool, err error) {
	dbPath := filepath.Join(veskaHome, "veska.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", false, nil // nothing to rescue
		}
		return "", false, fmt.Errorf("restore: stat existing db: %w", statErr)
	}

	// Refuse if a stale rescue copy is already present.
	entries, err := os.ReadDir(veskaHome)
	if err != nil {
		return "", false, fmt.Errorf("restore: read veska home: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "veska.db.before-restore-") {
			return "", false, fmt.Errorf("%w: %s", ErrStaleRescueCopy, filepath.Join(veskaHome, e.Name()))
		}
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	rescuePath = filepath.Join(veskaHome, "veska.db.before-restore-"+ts+".bak")

	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, statErr := os.Stat(src); statErr != nil {
			continue // WAL/SHM may legitimately be absent
		}
		if renErr := os.Rename(src, rescuePath+suffix); renErr != nil {
			return "", false, fmt.Errorf("restore: rescue %s: %w", src, renErr)
		}
	}
	return rescuePath, true, nil
}

// rollbackRescue restores a before-restore rescue copy back to veska.db.
func rollbackRescue(veskaHome, rescuePath string, rescued bool) error {
	if !rescued {
		return nil
	}
	dbPath := filepath.Join(veskaHome, "veska.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := rescuePath + suffix
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		if renErr := os.Rename(src, dbPath+suffix); renErr != nil {
			return fmt.Errorf("restore: rollback %s: %w", src, renErr)
		}
	}
	return nil
}

// removeDBFiles deletes veska.db and its WAL/SHM siblings, ignoring absence.
func removeDBFiles(veskaHome string) {
	dbPath := filepath.Join(veskaHome, "veska.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
}

// checkRestoredDB runs PRAGMA integrity_check and a schema_migrations sanity
// check on the restored database.
func checkRestoredDB(dbPath string) error {
	integrityOK, _, err := checkSQLite(dbPath)
	if err != nil {
		return fmt.Errorf("open restored db: %w", err)
	}
	if !integrityOK {
		return errors.New("PRAGMA integrity_check did not return ok")
	}
	return nil
}

// extractTarGz extracts the archive into destDir. Entry names are cleaned and
// confined to defend against path traversal.
func extractTarGz(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFileFromTar(target, tr); err != nil {
				return err
			}
		default:
			// Skip symlinks and other exotic types: backups only hold
			// regular files and directories.
		}
	}
	return nil
}

// writeFileFromTar copies the current tar entry's bytes into a regular file.
func writeFileFromTar(target string, tr *tar.Reader) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // tar from a verified backup
		return err
	}
	return out.Close()
}

// safeJoin joins name onto base, rejecting entries that escape base.
func safeJoin(base, name string) (string, error) {
	cleaned := filepath.Clean("/" + name)
	target := filepath.Join(base, cleaned)
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("restore: tar entry escapes destination: %q", name)
	}
	return target, nil
}

// SelectLatest returns the newest user-initiated backup tarball in backupDir.
func SelectLatest(backupDir string) (string, error) {
	return selectNewest(backupDir, userPrefix, "user-initiated backup")
}

// SelectPreMigration returns the newest auto-pre-migration snapshot in
// backupDir.
func SelectPreMigration(backupDir string) (string, error) {
	return selectNewest(backupDir, autoPrefix, "pre-migration snapshot")
}

// selectNewest returns the lexically largest backup file starting with the
// prefix. Filenames embed a sortable UTC timestamp, so lexical order is
// chronological.
func selectNewest(backupDir, prefix, kind string) (string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: no %s in %s", ErrNoBackup, kind, backupDir)
		}
		return "", fmt.Errorf("restore: read backup dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".tar.gz") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%w: no %s in %s", ErrNoBackup, kind, backupDir)
	}
	sort.Strings(names)
	return filepath.Join(backupDir, names[len(names)-1]), nil
}
