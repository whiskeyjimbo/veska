package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/crashloop"
)

func newRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "engram-daemon",
		Short:        "Engram long-running daemon (supervises Dolt + Qdrant)",
		SilenceUsage: true,
	}
}

func main() {
	// Crash-loop guard: refuse to start if the breaker has tripped.
	if err := crashloop.Check(config.DefaultVectorDir()); err != nil {
		if errors.Is(err, crashloop.ErrBroken) {
			fmt.Fprintln(os.Stderr, "engram-daemon: crash-loop breaker tripped — run `engram doctor reset-crash-loop` to recover")
			os.Exit(crashloop.ExitCode)
		}
		fmt.Fprintf(os.Stderr, "engram-daemon: crash-loop check failed: %v\n", err)
		os.Exit(1)
	}

	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
