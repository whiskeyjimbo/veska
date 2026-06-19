// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PruneOptions controls a retention sweep over a backup directory.
type PruneOptions struct {
	// BackupDir is the directory holding backup tarballs.
	BackupDir string
	// KeepMinCount is the number of most-recent user-initiated backups always
	// kept regardless of age (config [backup].keep_min_count, default 3).
	KeepMinCount int
	// MaxAge deletes user-initiated backups older than this, subject to
	// KeepMinCount. Derived from [backup].keep_max_age.
	MaxAge time.Duration
	// Now is the reference time for age comparisons; zero means time.Now.
	Now time.Time
}

// PruneResult summarizes a retention sweep.
type PruneResult struct {
	// Deleted is the list of tarball paths removed.
	Deleted []string
	// Kept is the count of user-initiated backups retained.
	Kept int
}

// Prune applies the retention policy to user-initiated backups. It keeps the
// KeepMinCount most-recent backups regardless of age and deletes the rest if
// they are older than MaxAge. Auto-pre-migration snapshots are never touched
// here. Prune is idempotent.
func Prune(opts PruneOptions) (PruneResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	entries, err := os.ReadDir(opts.BackupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return PruneResult{}, nil // nothing to prune
		}
		return PruneResult{}, fmt.Errorf("prune: read backup dir: %w", err)
	}

	// Collect user-initiated backups only; skip auto-pre-migration snapshots.
	type backupFile struct {
		name    string
		modTime time.Time
	}
	var files []backupFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, userPrefix) || !strings.HasSuffix(n, ".tar.gz") {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			return PruneResult{}, fmt.Errorf("prune: stat %s: %w", n, statErr)
		}
		files = append(files, backupFile{name: n, modTime: info.ModTime()})
	}

	// Newest first: filenames embed a sortable UTC timestamp.
	sort.Slice(files, func(i, j int) bool { return files[i].name > files[j].name })

	result := PruneResult{}
	for i, f := range files {
		// Always keep the KeepMinCount most-recent backups.
		if i < opts.KeepMinCount {
			result.Kept++
			continue
		}
		// Older ones survive only if within MaxAge.
		if opts.MaxAge > 0 && now.Sub(f.modTime) <= opts.MaxAge {
			result.Kept++
			continue
		}
		path := filepath.Join(opts.BackupDir, f.name)
		if err := os.Remove(path); err != nil {
			return result, fmt.Errorf("prune: remove %s: %w", path, err)
		}
		result.Deleted = append(result.Deleted, path)
	}
	return result, nil
}

// ParseRetentionAge parses a retention duration string. It accepts Go durations
// and a custom day suffix 'd', which time.ParseDuration does not natively support.
func ParseRetentionAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, fmt.Errorf("parse retention age %q: %w", s, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse retention age %q: %w", s, err)
	}
	return d, nil
}
