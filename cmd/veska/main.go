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
		Use:          "veska",
		Short:        "Veska code intelligence CLI",
		SilenceUsage: true,
	}
	root.AddCommand(initCmd())
	root.AddCommand(hookRunnerCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(backupCmd())
	root.AddCommand(restoreCmd())
	root.AddCommand(repoCmd(nil))
	root.AddCommand(wikiCmd())
	root.AddCommand(reindexCmd())

	// Resolve the daemon binary path at startup. os.Executable returns the path
	// of the current binary; by convention the daemon lives alongside the CLI
	// with the name "veska-daemon".
	var mgr service.Manager
	if exe, err := os.Executable(); err == nil {
		mgr, _ = service.New(exe+"-daemon", config.DefaultVectorDir())
	}
	root.AddCommand(serviceCmd(mgr))
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
