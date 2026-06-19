// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/configcmd"
	"github.com/whiskeyjimbo/veska/internal/platform/service"
)

// configCmd is the parent for `veska config …`.
// opt-in features that need the [vuln_source] block in
// ~/.veska/config.toml require a daemon restart AND a re-scan of every
// already-promoted repo to surface new findings retroactively. Without
// this command a user has to chain three separate calls
// (service stop → service start → reindex <path> for every repo). The
// reload subcommand turns that into one.
func configCmd(mgr service.Manager) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Manage veska configuration",
		SilenceUsage: true,
	}
	cmd.AddCommand(configReloadCmd(mgr))
	cmd.AddCommand(configShowCmd())
	return cmd
}

// configShowCmd prints the effective resolved config: defaults merged with
// ~/.veska/config.toml and env-var overrides - same pipeline the daemon
// uses at boot, so the operator sees the EXACT shape the daemon will
// observe. Read-only; write-side subcommands (set/enable/disable) are deferred because
// BurntSushi/toml v1.6 loses comments on marshal.
func configShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "show",
		Short:        "Print the effective veska configuration (defaults + config.toml + env)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return configcmd.RunShow(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of TOML")
	return cmd
}

func configReloadCmd(mgr service.Manager) *cobra.Command {
	return &cobra.Command{
		Use:          "reload",
		Short:        "Restart the daemon and re-promote every registered repo so new config takes effect",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return configcmd.RunReload(cmd.Context(), configcmd.ReloadParams{
				Manager:     mgr,
				Out:         cmd.OutOrStdout(),
				DaemonReady: daemonRunning,
			})
		},
	}
}
