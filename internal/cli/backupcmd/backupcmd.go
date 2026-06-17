// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package backupcmd holds the business logic behind the `veska backup` command
// family. cmd/veska/backup.go is reduced to Cobra construction whose RunE bodies
// delegate here, following the cmd = glue / logic-in-packages pattern
// (). `list` is pure on-disk reporting (this file);
// create/verify/prune (mutate.go) orchestrate config + the
// internal/infrastructure/backup primitives.
package backupcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// ListParams bundles the inputs of RunList.
type ListParams struct {
	// BackupDir is the explicit --backup-dir value; "" triggers ResolveDir.
	BackupDir string
	JSONOut   bool
	Out       io.Writer
	// ResolveDir returns the directory to read when BackupDir is empty
	// (canonical $VESKA_HOME/backups with legacy fallback). Injected so the
	// package stays free of config/$VESKA_HOME resolution.
	ResolveDir func() (string, error)
	// FormatBytes renders a byte count for the human table (cmd/veska's
	// humanBytes), injected to keep one formatter across backup and savings.
	FormatBytes func(int64) string
}

// row is one backup tarball as reported by RunList.
type row struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size_bytes"`
	ModTime time.Time `json:"mtime"`
	Kind    string    `json:"kind"` // "user" or "pre-migration"
}

// RunList lists the tarballs in the backup directory, newest-first, with size,
// mtime, and kind (user-initiated vs. auto pre-migration snapshot). It only
// reports what's on disk; retention semantics live in the prune path.
func RunList(p ListParams) error {
	dir := p.BackupDir
	if dir == "" {
		resolved, err := p.ResolveDir()
		if err != nil {
			return fmt.Errorf("backup list: %w", err)
		}
		dir = resolved
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if p.JSONOut {
				return json.NewEncoder(p.Out).Encode(struct {
					BackupDir string `json:"backup_dir"`
					Backups   []any  `json:"backups"`
				}{dir, nil})
			}
			fmt.Fprintf(p.Out, "no backups: %s does not exist\n", dir)
			return nil
		}
		return fmt.Errorf("backup list: %w", err)
	}

	rows := collectRows(dir, entries)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ModTime.After(rows[j].ModTime) })

	if p.JSONOut {
		return json.NewEncoder(p.Out).Encode(struct {
			BackupDir string `json:"backup_dir"`
			Backups   []row  `json:"backups"`
		}{dir, rows})
	}
	if len(rows) == 0 {
		fmt.Fprintf(p.Out, "no backups in %s\n", dir)
		return nil
	}
	fmt.Fprintf(p.Out, "backups in %s:\n", dir)
	tw := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tSIZE\tMTIME")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Kind, p.FormatBytes(r.Size), r.ModTime.UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}

// collectRows filters dir entries down to recognised backup tarballs, tagging
// each with its kind. Unrecognised files and directories are skipped.
func collectRows(dir string, entries []os.DirEntry) []row {
	var rows []row
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		var kind string
		switch {
		case strings.HasPrefix(name, "veska-backup-"):
			kind = "user"
		case strings.HasPrefix(name, "auto-pre-migration-"):
			kind = "pre-migration"
		default:
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, row{
			Name:    name,
			Path:    filepath.Join(dir, name),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Kind:    kind,
		})
	}
	return rows
}
