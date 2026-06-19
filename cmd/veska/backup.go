// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/backupcmd"
)

// The backup command family's logic lives in internal/cli/backupcmd; these
// constructors are Cobra glue whose RunE bodies delegate there.

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

// backupListCmd lists the tarballs in the backup directory, newest-first, with
// size, mtime, and kind (user-initiated vs. auto pre-migration snapshot).
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

func backupPruneCmd() *cobra.Command {
	var backupDir string
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Apply the backup retention policy, deleting old backups",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return backupcmd.RunPrune(cmd.OutOrStdout(), backupDir)
		},
	}
	cmd.Flags().StringVar(&backupDir, "backup-dir", "", "directory to prune (default: $VESKA_HOME/backups)")
	return cmd
}

// backupCreateCmd returns the "backup create" subcommand that runs VACUUM INTO,
// copies supporting files, and writes a timestamped.tar.gz to --output-dir.
func backupCreateCmd() *cobra.Command {
	var outputDir string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the veska database and supporting files",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return backupcmd.RunCreate(cmd.OutOrStdout(), outputDir)
		},
	}
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the backup tarball (default: $VESKA_HOME/backups)")
	return cmd
}

// backupVerifyCmd returns the "backup verify" subcommand. It extracts veska.db
// from the tarball, runs PRAGMA integrity_check and foreign_key_check, and
// validates audit.jsonl if present.
// Exit codes follow:
//
//	0 = healthy
//	1 = degraded (audit.jsonl malformed but DB ok)
//	2 = broken (DB checks failed or archive unreadable)
func backupVerifyCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "verify <path>",
		Short:        "Verify the integrity of a backup tarball",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return backupcmd.RunVerify(cmd.OutOrStdout(), args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output result as JSON envelope")
	return cmd
}
