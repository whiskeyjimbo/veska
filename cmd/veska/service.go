package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/platform/service"
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
// mgr is the real-side manager that actually mutates supervisor state.
// dryMgr is the dry-run-mode sibling: every mutating call on it prints the
// concrete file paths and supervisor commands it WOULD run.
// Both may be nil; every subcommand guards against a nil manager and
// returns a descriptive error rather than panicking.
func serviceCmd(mgr, dryMgr ServiceManager) *cobra.Command {
	root := &cobra.Command{
		Use:          "service",
		Short:        "Manage the veska daemon OS service (install, start, stop, …)",
		SilenceUsage: true,
	}

	// reportStarted polls Status briefly and prints the post-start state;
	// shared verbatim by `start` and `restart`.
	reportStarted := func(cmd *cobra.Command, m ServiceManager, dryRun bool) error {
		if !dryRun {
			printServiceStarted(cmd, m)
		}
		return nil
	}

	root.AddCommand(
		newServiceSubcmd(mgr, dryMgr, "install", "Install the veska daemon as an OS service",
			mutateThenReport(ServiceManager.Install, "service installed (run 'veska service start' to start the daemon)")),
		newServiceSubcmd(mgr, dryMgr, "uninstall", "Uninstall the veska daemon OS service",
			mutateThenReport(ServiceManager.Uninstall, "service uninstalled")),
		newServiceSubcmd(mgr, dryMgr, "start", "Start the veska daemon OS service",
			mutateThenAct(ServiceManager.Start, reportStarted)),
		newServiceSubcmd(mgr, dryMgr, "stop", "Stop the veska daemon OS service",
			mutateThenReport(ServiceManager.Stop, "service stopped")),
		newServiceSubcmd(mgr, dryMgr, "restart", "Restart the veska daemon OS service",
			mutateThenAct(ServiceManager.Restart, reportStarted)),
		newServiceSubcmd(mgr, dryMgr, "status", "Show the current state of the veska daemon OS service",
			runServiceStatus),
	)

	return root
}

// serviceAction runs a service subcommand against the already-resolved,
// non-nil manager. dryRun is threaded through so actions can suppress the
// success confirmation in dry-run mode (the dry-run manager prints the
// would-do lines itself).
type serviceAction func(cmd *cobra.Command, mgr ServiceManager, dryRun bool) error

// newServiceSubcmd builds one `veska service <verb>` subcommand, centralising
// the parts every verb shares: the --dry-run flag, --dry-run-aware manager
// selection, and the nil-manager guard. Only the verb-specific action differs.
func newServiceSubcmd(mgr, dryMgr ServiceManager, use, short string, action serviceAction) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        short,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			useMgr := selectManager(mgr, dryMgr, dryRun)
			if useMgr == nil {
				return errNoManager
			}
			return action(cmd, useMgr, dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done without making changes")
	return cmd
}

// mutateThenAct returns a serviceAction that runs the mutating manager call,
// then hands off to report on success. Used by start/restart whose post-action
// step is the Status-polling printServiceStarted rather than a static message.
func mutateThenAct(mutate func(ServiceManager, context.Context) error, report serviceAction) serviceAction {
	return func(cmd *cobra.Command, m ServiceManager, dryRun bool) error {
		if err := mutate(m, cmd.Context()); err != nil {
			return err
		}
		return report(cmd, m, dryRun)
	}
}

// mutateThenReport returns a serviceAction that runs the mutating manager call
// and, on success outside dry-run mode, prints a one-line confirmation. Covers
// the install/uninstall/stop verbs whose only post-action work is that message.
func mutateThenReport(mutate func(ServiceManager, context.Context) error, msg string) serviceAction {
	return mutateThenAct(mutate, func(cmd *cobra.Command, _ ServiceManager, dryRun bool) error {
		if !dryRun {
			fmt.Fprintln(cmd.OutOrStdout(), msg)
		}
		return nil
	})
}

// runServiceStatus prints the current supervisor state. Unlike the mutating
// verbs it reports unconditionally (dry-run and real runs alike) and has no
// success message of its own.
func runServiceStatus(cmd *cobra.Command, m ServiceManager, _ bool) error {
	st, err := m.Status(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "service status: running=%v pid=%d message=%q\n",
		st.Running, st.PID, st.Message)
	return nil
}

// selectManager returns the manager that should service a request given the
// dry-run flag. mgr/dryMgr may be nil; selectManager falls back to mgr when
// dryMgr wasn't wired, so dry-run still does something useful in test
// callsites that only supply one.
func selectManager(mgr, dryMgr ServiceManager, dryRun bool) ServiceManager {
	if dryRun && dryMgr != nil {
		return dryMgr
	}
	return mgr
}

// printServiceStarted reports the post-start state by polling Status briefly.
// The supervisor returns from start before the unit transitions to active, so
// without a short wait the pid is still 0 and the message reads "activating".
func printServiceStarted(cmd *cobra.Command, mgr ServiceManager) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := mgr.Status(cmd.Context())
		if err == nil && st.Running && st.PID != 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "service started: pid=%d (run 'veska service status' to verify)\n", st.PID)
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(cmd.OutOrStdout(), "service start requested (run 'veska service status' to verify)")
			return
		}
		select {
		case <-time.After(150 * time.Millisecond):
		case <-cmd.Context().Done():
			return
		}
	}
}
