package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "engram",
		Short:        "Engram code intelligence CLI",
		SilenceUsage: true,
	}
	root.AddCommand(hookRunnerCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(serviceCmd(nil))
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
