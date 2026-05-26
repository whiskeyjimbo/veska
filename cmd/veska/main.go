package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/service"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "veska",
		Short:         "Veska code intelligence CLI",
		Version:       shortVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Compact one-liner for `veska --version`; `veska version` keeps the
	// full multi-line build info dump (solov2-fy14).
	root.SetVersionTemplate("veska {{.Version}}\n")
	root.AddCommand(initCmd())
	root.AddCommand(hookRunnerCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(backupCmd())
	root.AddCommand(restoreCmd())
	root.AddCommand(repoCmd())
	root.AddCommand(wikiCmd())
	root.AddCommand(reindexCmd())
	root.AddCommand(searchCmd())
	root.AddCommand(symbolCmd())
	root.AddCommand(contextCmd())
	root.AddCommand(findingsCmd())
	root.AddCommand(installCmd())
	root.AddCommand(versionCmd())
	// Top-level alias for `veska doctor savings` so the marketing-y
	// shortcut works without the doctor prefix (solov2-3bu).
	root.AddCommand(doctorSavingsCmd())

	// Resolve the daemon binary path at startup. os.Executable returns the path
	// of the current binary; by convention the daemon lives alongside the CLI
	// with the name "veska-daemon".
	var mgr, dryMgr service.Manager
	if exe, err := os.Executable(); err == nil {
		mgr, _ = service.New(exe+"-daemon", config.DefaultVectorDir())
		dryMgr, _ = service.NewDryRun(exe+"-daemon", config.DefaultVectorDir())
	}
	root.AddCommand(serviceCmd(mgr, dryMgr))
	root.AddCommand(configCmd(mgr))
	root.AddCommand(upgradeCmd(mgr))
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var pse ProbeStatusError
		if isProbeStatusError(err, &pse) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitCodeForProbeStatus(pse.Status))
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
