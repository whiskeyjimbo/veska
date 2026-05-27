package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/backup"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/doctor"
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
			w := cmd.OutOrStdout()
			if backupDir == "" {
				dir, err := defaultBackupDir()
				if err != nil {
					return fmt.Errorf("backup list: %w", err)
				}
				backupDir = dir
			}
			entries, err := os.ReadDir(backupDir)
			if err != nil {
				if os.IsNotExist(err) {
					if jsonOut {
						return json.NewEncoder(w).Encode(struct {
							BackupDir string `json:"backup_dir"`
							Backups   []any  `json:"backups"`
						}{backupDir, nil})
					}
					fmt.Fprintf(w, "no backups: %s does not exist\n", backupDir)
					return nil
				}
				return fmt.Errorf("backup list: %w", err)
			}
			type row struct {
				Name    string    `json:"name"`
				Path    string    `json:"path"`
				Size    int64     `json:"size_bytes"`
				ModTime time.Time `json:"mtime"`
				Kind    string    `json:"kind"` // "user" or "pre-migration"
			}
			var rows []row
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if !strings.HasSuffix(name, ".tar.gz") {
					continue
				}
				kind := ""
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
					Path:    filepath.Join(backupDir, name),
					Size:    info.Size(),
					ModTime: info.ModTime(),
					Kind:    kind,
				})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].ModTime.After(rows[j].ModTime) })
			if jsonOut {
				return json.NewEncoder(w).Encode(struct {
					BackupDir string `json:"backup_dir"`
					Backups   []row  `json:"backups"`
				}{backupDir, rows})
			}
			if len(rows) == 0 {
				fmt.Fprintf(w, "no backups in %s\n", backupDir)
				return nil
			}
			fmt.Fprintf(w, "backups in %s:\n", backupDir)
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tSIZE\tMTIME")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Kind, humanSize(r.Size), r.ModTime.UTC().Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "directory to list (default: ~/.veska-backups)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

// humanSize renders a byte count in a compact human-readable form.
func humanSize(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%dB", n)
	case n < k*k:
		return fmt.Sprintf("%.1fKB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1fMB", float64(n)/k/k)
	default:
		return fmt.Sprintf("%.1fGB", float64(n)/k/k/k)
	}
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

	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "directory to prune (default: ~/.veska-backups)")
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
				homeDir, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("backup create: resolve home dir: %w", err)
				}
				outputDir = filepath.Join(homeDir, ".veska-backups")
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

	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the backup tarball (default: ~/.veska-backups)")
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
