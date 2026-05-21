package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/service"
)

// ServiceManager is the port through which the service subcommands drive the
// platform-specific supervisor (systemd --user on Linux, launchd on macOS).
// It is an alias for service.Manager so callers in this package need not import
// the service package directly.
type ServiceManager = service.Manager

// ServiceStatus describes the current state of the veska daemon service.
// It is an alias for service.ServiceStatus.
type ServiceStatus = service.ServiceStatus

// errNoManager is returned when a service subcommand is invoked but no real
// ServiceManager implementation has been wired in (e.g. during early bootstrap).
var errNoManager = errors.New("service manager not available")

// serviceCmd returns the "service" Cobra command tree.
//
// mgr is the real-side manager that actually mutates supervisor state.
// dryMgr is the dry-run-mode sibling: every mutating call on it prints the
// concrete file paths and supervisor commands it WOULD run (solov2-kqp).
// Both may be nil; every subcommand guards against a nil manager and
// returns a descriptive error rather than panicking.
func serviceCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	root := &cobra.Command{
		Use:          "service",
		Short:        "Manage the veska daemon OS service (install, start, stop, …)",
		SilenceUsage: true,
	}

	root.AddCommand(
		serviceInstallCmd(mgr, dryMgr),
		serviceUninstallCmd(mgr, dryMgr),
		serviceStartCmd(mgr, dryMgr),
		serviceStopCmd(mgr, dryMgr),
		serviceRestartCmd(mgr, dryMgr),
		serviceStatusCmd(mgr, dryMgr),
	)

	return root
}

// pick returns the manager that should service a request given the
// --dry-run flag. mgr/dryMgr may be nil; pick falls back to mgr when
// dryMgr wasn't wired, so dry-run still does something useful in test
// callsites that only supply one.
func pick(mgr, dryMgr ServiceManager, dryRun bool) ServiceManager {
	if dryRun && dryMgr != nil {
		return dryMgr
	}
	return mgr
}

func serviceInstallCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install the veska daemon as an OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return useMgr.Install(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceUninstallCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "uninstall",
		Short:        "Uninstall the veska daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return useMgr.Uninstall(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStartCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "start",
		Short:        "Start the veska daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return useMgr.Start(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStopCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "stop",
		Short:        "Stop the veska daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return useMgr.Stop(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceRestartCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "restart",
		Short:        "Restart the veska daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return useMgr.Restart(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

func serviceStatusCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show the current state of the veska daemon OS service",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := pick(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			st, err := useMgr.Status(cmd.Context())
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
