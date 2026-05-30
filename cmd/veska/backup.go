package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/backupcmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// backupCmd returns the top-level "backup" Cobra command with subcommands.
func backupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "backup",
		Short:        "Manage veska backups",
		SilenceUsage: true,
	}
	cmd.AddCommand(backupCreateCmd())
	cmd.AddCommand(backupVerifyCmd())
	cmd.AddCommand(backupPruneCmd())
	cmd.AddCommand(backupListCmd())
	return cmd
}

// backupListCmd lists the tarballs in ~/.veska-backups, sorted newest-first,
// with size, mtime, and kind (user-initiated vs. auto pre-migration
// snapshot). Solo-17 §4.5 retention semantics decide which kind each
// belongs to; this command only reports what's on disk (solov2-k1n9).
func backupListCmd() *cobra.Command {
	var (
		backupDir string
		jsonOut   bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List backup tarballs in the backup directory, newest first",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return backupcmd.RunList(backupcmd.ListParams{
				BackupDir:   backupDir,
				JSONOut:     jsonOut,
				Out:         cmd.OutOrStdout(),
				ResolveDir:  resolveBackupReadDir,
				FormatBytes: humanBytes,
			})
		},
	}
	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "directory to list (default: $VESKA_HOME/backups, falling back to ~/.veska-backups when empty)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

// backupPruneCmd returns the "backup prune" subcommand.  It applies the
// SOLO-17 §4.5 retention policy over ~/.veska-backups: it keeps the
// [backup].keep_min_count most-recent user-initiated backups regardless of
// age and deletes the rest if they are older than [backup].keep_max_age.
// Auto-pre-migration snapshots are never pruned.  Idempotent.
func backupPruneCmd() *cobra.Command {
	var backupDir string

	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Apply the backup retention policy, deleting old backups",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()

			if backupDir == "" {
				// solov2-n57f: prune the canonical location;
				// legacy ~/.veska-backups stays untouched unless --backup-dir
				// names it explicitly. Retention runs on the dir that NEW
				// tarballs land in, not on stale legacy state.
				dir, err := defaultBackupDir()
				if err != nil {
					return fmt.Errorf("backup prune: %w", err)
				}
				backupDir = dir
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

			fmt.Fprintf(w, "backup prune: kept %d, deleted %d\n", result.Kept, len(result.Deleted))
			for _, p := range result.Deleted {
				fmt.Fprintf(w, "  deleted %s\n", p)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "directory to prune (default: $VESKA_HOME/backups)")
	return cmd
}

// backupCreateCmd returns the "backup create" subcommand that runs VACUUM INTO,
// copies supporting files, and writes a timestamped .tar.gz to --output-dir.
func backupCreateCmd() *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the veska database and supporting files",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()

			veskaHome := config.DefaultVectorDir()
			dbPath := filepath.Join(veskaHome, "veska.db")

			if outputDir == "" {
				// solov2-n57f: new tarballs go under $VESKA_HOME/backups
				// so a single `rm -rf $VESKA_HOME` clears all veska state.
				outputDir = config.DefaultBackupDir()
			}

			result, err := backup.Create(backup.CreateOptions{
				DBPath:    dbPath,
				VeskaHome: veskaHome,
				BackupDir: outputDir,
			})
			if err != nil {
				return fmt.Errorf("backup create: %w", err)
			}

			fmt.Fprintf(w, "backup created: %s (%d bytes)\n", result.Path, result.SizeBytes)
			return nil
		},
	}

	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the backup tarball (default: $VESKA_HOME/backups)")
	return cmd
}

// backupVerifyCmd returns the "backup verify" subcommand.  It extracts
// veska.db from the tarball, runs PRAGMA integrity_check and
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

			// solov2-tkqd: resolve a bare backup name (the NAME column from
			// `backup list`) against the configured backups dir, then stat
			// the file before calling Verify. Otherwise Verify's "couldn't
			// open the archive" failure was rendered as "broken
			// (db_integrity=false, ...)" — indistinguishable from a real
			// corrupt-DB result, which made fat-fingered paths look like
			// healthy backups had gone bad.
			if _, statErr := os.Stat(tarPath); statErr != nil && !filepath.IsAbs(tarPath) && !strings.ContainsRune(tarPath, filepath.Separator) {
				if dir, derr := defaultBackupDir(); derr == nil {
					candidate := filepath.Join(dir, tarPath)
					if _, csErr := os.Stat(candidate); csErr == nil {
						tarPath = candidate
					}
				}
			}
			if _, statErr := os.Stat(tarPath); statErr != nil {
				return fmt.Errorf("backup verify: %w", statErr)
			}

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
