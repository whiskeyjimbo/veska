package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/backup"
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/doctor"
)

// backupCmd returns the top-level "backup" Cobra command with subcommands.
func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "backup",
		Short:        "Manage engram backups",
		SilenceUsage: true,
	}
	cmd.AddCommand(backupCreateCmd())
	cmd.AddCommand(backupVerifyCmd())
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

// backupVerifyCmd returns the "backup verify" subcommand.  It extracts
// engram.db from the tarball, runs PRAGMA integrity_check and
// PRAGMA foreign_key_check, and validates audit.jsonl if present.
//
// Exit codes follow SOLO-13 §2:
//
//	0 = healthy
//	1 = degraded (audit.jsonl malformed but DB ok)
//	2 = broken   (DB checks failed or archive unreadable)
func backupVerifyCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:          "verify <path>",
		Short:        "Verify the integrity of a backup tarball",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			tarPath := args[0]

			result, err := backup.Verify(tarPath)
			if err != nil {
				return fmt.Errorf("backup verify: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("backup_verify", result.Status, result))
			}

			fmt.Fprintf(w, "backup verify: %s (db_integrity=%v, foreign_key=%v, audit_present=%v, audit_ok=%v)\n",
				result.Status, result.DBIntegrityOK, result.ForeignKeyOK, result.AuditPresent, result.AuditJSONLOK)

			if result.Status != "healthy" {
				return ProbeStatusError{Subsystem: "backup_verify", Status: result.Status}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "output result as JSON envelope")
	return cmd
}
