package main

import (
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/restorecmd"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// The restore command's logic lives in internal/cli/restorecmd; this
// constructor is Cobra glue whose RunE body delegates there (solov2-0omh.10).

// restoreCmd returns the "restore" Cobra command. It restores a backup tarball
// into the veska home (SOLO-17 §4.4). The daemon must be stopped.
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
			var path string
			if len(args) == 1 {
				path = args[0]
			}
			return restorecmd.Run(restorecmd.Params{
				Path:            path,
				UseLatest:       useLatest,
				UsePreMigration: usePreMigration,
				Out:             cmd.OutOrStdout(),
				DaemonRunning:   daemonRunning,
				ResolveReadDir:  resolveBackupReadDir,
				VeskaHome:       config.DefaultVectorDir(),
			})
		},
	}

	cmd.Flags().BoolVar(&useLatest, "latest", false, "restore the newest backup tarball in $VESKA_HOME/backups (falls back to ~/.veska-backups)")
	cmd.Flags().BoolVar(&usePreMigration, "pre-migration", false, "restore the newest auto-pre-migration snapshot")
	return cmd
}
