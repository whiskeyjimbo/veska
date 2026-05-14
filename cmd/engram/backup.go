package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/backup"
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
)

// backupCmd returns the top-level "backup" Cobra command with subcommands.
func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "backup",
		Short:        "Manage engram backups",
		SilenceUsage: true,
	}
	cmd.AddCommand(backupCreateCmd())
	return cmd
}

// backupCreateCmd returns the "backup create" subcommand that runs VACUUM INTO,
// copies supporting files, and writes a timestamped .tar.gz to --output-dir.
func backupCreateCmd() *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the engram database and supporting files",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()

			engramHome := config.DefaultVectorDir()
			dbPath := filepath.Join(engramHome, "engram.db")

			if outputDir == "" {
				homeDir, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("backup create: resolve home dir: %w", err)
				}
				outputDir = filepath.Join(homeDir, ".engram-backups")
			}

			result, err := backup.Create(backup.CreateOptions{
				DBPath:     dbPath,
				EngramHome: engramHome,
				BackupDir:  outputDir,
			})
			if err != nil {
				return fmt.Errorf("backup create: %w", err)
			}

			fmt.Fprintf(w, "backup created: %s (%d bytes)\n", result.Path, result.SizeBytes)
			return nil
		},
	}

	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the backup tarball (default: ~/.engram-backups)")
	return cmd
}
