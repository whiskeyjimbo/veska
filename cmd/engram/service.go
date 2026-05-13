package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/service"
)

// ServiceManager is the port through which the service subcommands drive the
// platform-specific supervisor (systemd --user on Linux, launchd on macOS).
// It is an alias for service.Manager so callers in this package need not import
// the service package directly.
type ServiceManager = service.Manager

// ServiceStatus describes the current state of the engram daemon service.
// It is an alias for service.ServiceStatus.
type ServiceStatus = service.ServiceStatus

// errNoManager is returned when a service subcommand is invoked but no real
// ServiceManager implementation has been wired in (e.g. during early bootstrap).
var errNoManager = errors.New("service manager not available")

// serviceCmd returns the "service" Cobra command tree.
// mgr may be nil; every subcommand guards against a nil manager and returns a
// descriptive error rather than panicking.
func serviceCmd(mgr ServiceManager) *cobra.Command {
	root := &cobra.Command{
		Use:          "service",
		Short:        "Manage the engram daemon OS service (install, start, stop, …)",
		SilenceUsage: true,
	}

	root.AddCommand(
		serviceInstallCmd(mgr),
		serviceUninstallCmd(mgr),
		serviceStartCmd(mgr),
		serviceStopCmd(mgr),
		serviceRestartCmd(mgr),
		serviceStatusCmd(mgr),
	)

	return root
}

func serviceInstallCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install the engram daemon as an OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would install service")
				return nil
			}
			return mgr.Install(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceUninstallCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "uninstall",
		Short:        "Uninstall the engram daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would uninstall service")
				return nil
			}
			return mgr.Uninstall(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStartCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start the engram daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would start service")
				return nil
			}
			return mgr.Start(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStopCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "stop",
		Short:        "Stop the engram daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would stop service")
				return nil
			}
			return mgr.Stop(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceRestartCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "restart",
		Short:        "Restart the engram daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would restart service")
				return nil
			}
			return mgr.Restart(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStatusCmd(mgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show the current state of the engram daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mgr == nil {
				return errNoManager
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry-run: would query service status")
				return nil
			}
			st, err := mgr.Status(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "service status: running=%v pid=%d message=%q\n",
				st.Running, st.PID, st.Message)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}
