// Package backup implements veska backup creation and verification.
// See SOLO-08 §9.2 for the full specification.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// CreateOptions controls backup creation behaviour.
type CreateOptions struct {
	// DBPath is the path to the live veska.db SQLite database.
	DBPath string
	// VeskaHome is the veska data directory (e.g. ~/.veska).
	VeskaHome string
	// BackupDir is the directory where the finished tarball is written.
	// Typically ~/.veska-backups.
	BackupDir string
}

// CreateResult is returned by Create on success.
type CreateResult struct {
	// Path is the absolute path to the written tarball.
	Path string
	// SizeBytes is the size of the tarball in bytes.
	SizeBytes int64
}

// Create produces a timestamped backup tarball in opts.BackupDir.
//
// Steps:
//  1. Creates a temp staging directory.
//  2. Runs VACUUM INTO <staging>/veska.db on opts.DBPath (read-only connection).
//  3. Copies audit.jsonl and rotations (.1–.5) from opts.VeskaHome if present.
//  4. Copies config.toml from opts.VeskaHome if present (silently skipped if absent).
//  5. Copies cache/ directory recursively if present.
//  6. Writes manifest.json with created_at, veska_home, go_version.
//  7. Creates <opts.BackupDir>/veska-backup-<timestamp>.tar.gz from staging.
//  8. Removes the staging directory.
//  9. Calls VerifyGzip on the finished tarball.
//
// 10. Returns CreateResult{Path, SizeBytes}.
func Create(opts CreateOptions) (CreateResult, error) {
	// 1. Staging directory.
	staging, err := os.MkdirTemp("", "veska-backup-*")
	if err != nil {
		return CreateResult{}, fmt.Errorf("backup: MkdirTemp: %w", err)
	}
	defer os.RemoveAll(staging)

	// 2. VACUUM INTO staging/veska.db.
	stagingDB := filepath.Join(staging, "veska.db")
	if err := vacuumInto(opts.DBPath, stagingDB); err != nil {
		return CreateResult{}, fmt.Errorf("backup: VACUUM INTO: %w", err)
	}

	// 3. Copy audit.jsonl + rotations.
	auditBase := filepath.Join(opts.VeskaHome, "audit.jsonl")
	if err := copyIfPresent(auditBase, filepath.Join(staging, "audit.jsonl")); err != nil {
		return CreateResult{}, fmt.Errorf("backup: copy audit.jsonl: %w", err)
	}
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("audit.jsonl.%d", i)
		src := filepath.Join(opts.VeskaHome, name)
		dst := filepath.Join(staging, name)
		if err := copyIfPresent(src, dst); err != nil {
			return CreateResult{}, fmt.Errorf("backup: copy %s: %w", name, err)
		}
	}

	// 4. Copy config.toml (skip silently if absent).
	configSrc := filepath.Join(opts.VeskaHome, "config.toml")
	if err := copyIfPresent(configSrc, filepath.Join(staging, "config.toml")); err != nil {
		return CreateResult{}, fmt.Errorf("backup: copy config.toml: %w", err)
	}

	// 5. Copy cache/ recursively if present.
	cacheSrc := filepath.Join(opts.VeskaHome, "cache")
	if _, err := os.Stat(cacheSrc); err == nil {
		cacheDst := filepath.Join(staging, "cache")
		if err := copyDirRecursive(cacheSrc, cacheDst); err != nil {
			return CreateResult{}, fmt.Errorf("backup: copy cache/: %w", err)
		}
	}

	// 6. Write manifest.json.
	manifest := map[string]string{
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"veska_home": opts.VeskaHome,
		"go_version": runtime.Version(),
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return CreateResult{}, fmt.Errorf("backup: marshal manifest: %w", err)
	}
	manifestPath := filepath.Join(staging, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o644); err != nil {
		return CreateResult{}, fmt.Errorf("backup: write manifest: %w", err)
	}

	// 7. Create tarball.
	if err := os.MkdirAll(opts.BackupDir, 0o755); err != nil {
		return CreateResult{}, fmt.Errorf("backup: mkdir backup dir: %w", err)
	}
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	tarName := fmt.Sprintf("veska-backup-%s.tar.gz", timestamp)
	tarPath := filepath.Join(opts.BackupDir, tarName)

	if err := createTarGz(tarPath, staging); err != nil {
		return CreateResult{}, fmt.Errorf("backup: create tar.gz: %w", err)
	}

	// 8. Staging cleanup happens via defer.

	// 9. Verify gzip.
	if err := VerifyGzip(tarPath); err != nil {
		return CreateResult{}, fmt.Errorf("backup: verify: %w", err)
	}

	// 10. Stat for size.
	info, err := os.Stat(tarPath)
	if err != nil {
		return CreateResult{}, fmt.Errorf("backup: stat tarball: %w", err)
	}

	return CreateResult{Path: tarPath, SizeBytes: info.Size()}, nil
}

// VerifyGzip opens path as a gzip stream and reads at least the first byte,
// confirming the archive is readable.  Returns nil on success.
func VerifyGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	buf := make([]byte, 1)
	_, err = gr.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

// vacuumInto opens the SQLite database at src read-only and runs VACUUM INTO dst.
func vacuumInto(src, dst string) error {
	db, err := sql.Open(sqldriver.Name, "file:"+src+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

// copyIfPresent copies src to dst.  If src does not exist the function returns
// nil (skip silently).  Parent directories of dst are created as needed.
func copyIfPresent(src, dst string) error {
	sf, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer sf.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	df, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer df.Close()

	_, err = io.Copy(df, sf)
	return err
}

// copyDirRecursive copies the directory tree rooted at src into dst.
func copyDirRecursive(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyIfPresent(path, target)
	})
}

// createTarGz walks srcDir and writes all files into a .tar.gz at tarPath.
func createTarGz(tarPath, srcDir string) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		hdr := &tar.Header{
			Name:    rel,
			Mode:    0o644,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		sf, err := os.Open(path)
		if err != nil {
			return err
		}
		defer sf.Close()

		_, err = io.Copy(tw, sf)
		return err
	})
	if err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}
