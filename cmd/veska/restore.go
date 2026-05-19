package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/backup"
	"github.com/whiskeyjimbo/veska/internal/config"
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

// defaultBackupDir returns the directory veska backup create writes to
// (~/.veska-backups), matching backupCreateCmd.
func defaultBackupDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(homeDir, ".veska-backups"), nil
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
				dir, err := defaultBackupDir()
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

	cmd.Flags().BoolVar(&useLatest, "latest", false, "restore the newest backup tarball in ~/.veska-backups")
	cmd.Flags().BoolVar(&usePreMigration, "pre-migration", false, "restore the newest auto-pre-migration snapshot")
	return cmd
}
