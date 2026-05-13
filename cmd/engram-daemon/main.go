package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "engram-daemon",
		Short:        "Engram long-running daemon (supervises Dolt + Qdrant)",
		SilenceUsage: true,
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
