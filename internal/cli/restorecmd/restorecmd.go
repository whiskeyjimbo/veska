// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package restorecmd holds the business logic behind the `veska restore`
// command. cmd/veska/restore.go is reduced to Cobra construction whose RunE
// body delegates here, following the cmd = glue / logic-in-packages pattern
// The mode-selection (exactly one of <path>/--latest/
// pre-migration), the daemon-running guard, and tarball resolution all live
// here so they are unit-testable without driving Cobra; $VESKA_HOME and
// backup-dir resolution stay in cmd/veska and are injected as seams.
package restorecmd

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
)

// Params bundles the inputs of Run.
type Params struct {
	// Path is the positional <path> argument; "" when not supplied.
	Path string
	// UseLatest selects the newest tarball in the backup read dir.
	UseLatest bool
	// UsePreMigration selects the newest auto-pre-migration snapshot.
	UsePreMigration bool
	Out             io.Writer
	// DaemonRunning reports whether the daemon is up; restore refuses to run
	// while it is. Injected (cmd/veska's daemonRunning) so the package stays
	// free of socket-dialing concerns.
	DaemonRunning func() bool
	// ResolveReadDir returns the directory to read for --latest (canonical
	// $VESKA_HOME/backups with legacy fallback). Injected to keep config /
	// $VESKA_HOME resolution in cmd/veska.
	ResolveReadDir func() (string, error)
	// VeskaHome is the restore target data root and the parent of the
	// pre-migration snapshot dir.
	VeskaHome string
}

// Run restores a backup tarball into VeskaHome. Exactly one
// selection mode must be set; the daemon must be stopped.
func Run(p Params) error {
	if err := p.checkExactlyOneMode(); err != nil {
		return err
	}
	if p.DaemonRunning() {
		return fmt.Errorf("restore: %w", backup.ErrDaemonRunning)
	}

	tarPath, err := p.resolveTarball()
	if err != nil {
		return err
	}

	result, err := backup.Restore(backup.RestoreOptions{
		TarballPath: tarPath,
		VeskaHome:   p.VeskaHome,
	})
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	fmt.Fprintf(p.Out, "restored from %s (db %d bytes)\n", result.TarballPath, result.DBSizeBytes)
	if result.RescuePath != "" {
		fmt.Fprintf(p.Out, "previous database rescued to %s\n", result.RescuePath)
	}
	return nil
}

// checkExactlyOneMode enforces that exactly one of <path>, --latest, or
// pre-migration was supplied.
func (p Params) checkExactlyOneMode() error {
	modes := 0
	if p.Path != "" {
		modes++
	}
	if p.UseLatest {
		modes++
	}
	if p.UsePreMigration {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("restore: provide exactly one of <path>, --latest, or --pre-migration")
	}
	return nil
}

// resolveTarball turns the selected mode into a concrete tarball path.
func (p Params) resolveTarball() (string, error) {
	switch {
	case p.Path != "":
		return p.Path, nil
	case p.UseLatest:
		// prefer the canonical $VESKA_HOME/backups but fall back
		// to ~/.veska-backups if the new dir is empty so users upgrading still
		// find their tarballs.
		dir, err := p.ResolveReadDir()
		if err != nil {
			return "", fmt.Errorf("restore: %w", err)
		}
		path, err := backup.SelectLatest(dir)
		if err != nil {
			return "", fmt.Errorf("restore: %w", err)
		}
		return path, nil
	default: // UsePreMigration
		path, err := backup.SelectPreMigration(filepath.Join(p.VeskaHome, "backups"))
		if err != nil {
			return "", fmt.Errorf("restore: %w", err)
		}
		return path, nil
	}
}
