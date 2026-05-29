package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// daemonRunning reports whether the veska daemon is up by dialing its CLI
// Unix socket. A successful connection means the daemon is listening.
func daemonRunning() bool {
	conn, err := net.DialTimeout("unix", config.CLISockPath(), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// defaultBackupDir returns the directory `veska backup create` writes to.
// solov2-n57f moved this under $VESKA_HOME/backups so wiping the data dir
// also clears backups (previously ~/.veska-backups survived the wipe).
func defaultBackupDir() (string, error) {
	return config.DefaultBackupDir(), nil
}

// resolveBackupReadDir returns the directory to READ backups from. Prefer
// the canonical $VESKA_HOME/backups; fall back to the legacy
// ~/.veska-backups when the canonical dir is missing or has no tarballs,
// so users upgrading still see backups they took under the old layout
// (solov2-n57f).
func resolveBackupReadDir() (string, error) {
	canon := config.DefaultBackupDir()
	if hasBackupTarballs(canon) {
		return canon, nil
	}
	if legacy, ok := config.LegacyBackupDir(); ok && hasBackupTarballs(legacy) {
		return legacy, nil
	}
	// No tarballs anywhere — return the canonical path so error messages
	// point at the location new writes will land.
	return canon, nil
}

// hasBackupTarballs reports whether dir contains at least one *.tar.gz.
// Used by resolveBackupReadDir to choose between the canonical and legacy
// locations.
func hasBackupTarballs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			return true
		}
	}
	return false
}

// restoreCmd returns the "restore" Cobra command.  It restores a backup
// tarball into the veska home (SOLO-17 §4.4).  The daemon must be stopped.
func restoreCmd() *cobra.Command {
	var useLatest, usePreMigration bool

	cmd := &cobra.Command{
		Use:   "restore [<path>]",
		Short: "Restore the veska database from a backup tarball",
		Long: "Restore the veska database from a backup tarball.\n\n" +
			"Provide an explicit <path>, or use --latest to select the newest\n" +
			"backup, or --pre-migration to select the newest auto-pre-migration\n" +
			"snapshot. The daemon must be stopped before restoring.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()

			// Exactly one selection mode.
			modes := 0
			if len(args) == 1 {
				modes++
			}
			if useLatest {
				modes++
			}
			if usePreMigration {
				modes++
			}
			if modes != 1 {
				return fmt.Errorf("restore: provide exactly one of <path>, --latest, or --pre-migration")
			}

			// Refuse if the daemon is running.
			if daemonRunning() {
				return fmt.Errorf("restore: %w", backup.ErrDaemonRunning)
			}

			// Resolve the tarball.
			var tarPath string
			switch {
			case len(args) == 1:
				tarPath = args[0]
			case useLatest:
				// solov2-n57f: prefer the canonical $VESKA_HOME/backups
				// but fall back to ~/.veska-backups if the new dir is
				// empty so users upgrading still find their tarballs.
				dir, err := resolveBackupReadDir()
				if err != nil {
					return fmt.Errorf("restore: %w", err)
				}
				p, err := backup.SelectLatest(dir)
				if err != nil {
					return fmt.Errorf("restore: %w", err)
				}
				tarPath = p
			case usePreMigration:
				veskaHome := config.DefaultVectorDir()
				p, err := backup.SelectPreMigration(filepath.Join(veskaHome, "backups"))
				if err != nil {
					return fmt.Errorf("restore: %w", err)
				}
				tarPath = p
			}

			result, err := backup.Restore(backup.RestoreOptions{
				TarballPath: tarPath,
				VeskaHome:   config.DefaultVectorDir(),
			})
			if err != nil {
				return fmt.Errorf("restore: %w", err)
			}

			fmt.Fprintf(w, "restored from %s (db %d bytes)\n", result.TarballPath, result.DBSizeBytes)
			if result.RescuePath != "" {
				fmt.Fprintf(w, "previous database rescued to %s\n", result.RescuePath)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&useLatest, "latest", false, "restore the newest backup tarball in $VESKA_HOME/backups (falls back to ~/.veska-backups)")
	cmd.Flags().BoolVar(&usePreMigration, "pre-migration", false, "restore the newest auto-pre-migration snapshot")
	return cmd
}
