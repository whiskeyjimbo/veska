package main

import (
	"context"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/upgradecmd"
)

// The upgrade command's logic lives in internal/cli/upgradecmd; this
// constructor is Cobra glue whose RunE body delegates there (solov2-0omh.11).

// upgradeCmd returns the "upgrade" Cobra command.
// mgr may be nil; --restart is only available when mgr is non-nil.
func upgradeCmd(mgr ServiceManager) *cobra.Command {
	var target string
	var restart bool

	cmd := &cobra.Command{
		Use:          "upgrade <path>",
		Short:        "Atomically replace the veska binary with a new build",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Inject only the manager's Restart method so upgradecmd stays
			// decoupled from the service package. nil mgr => nil RestartFn =>
			// --restart reports ErrNoManager.
			var restartFn func(context.Context) error
			if mgr != nil {
				restartFn = mgr.Restart
			}
			return upgradecmd.Run(cmd.Context(), upgradecmd.Params{
				Source:    args[0],
				Target:    target,
				Restart:   restart,
				Out:       cmd.OutOrStdout(),
				RestartFn: restartFn,
			})
		},
	}

	cmd.Flags().StringVar(&target, "target", "", "binary path to replace (default: current executable)")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart the daemon service after swapping the binary")

	return cmd
}
