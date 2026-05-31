package main

import (
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/doctorcmd"
	"github.com/whiskeyjimbo/veska/internal/cli/savingscmd"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// The doctor command tree's logic lives in internal/cli/doctorcmd; the
// constructors below are Cobra glue whose RunE bodies are thin delegating
// calls into that package (solov2-0omh.6).

// ProbeStatusError is returned by doctor subcommands when a probe yields a
// non-healthy status. main() translates it to the appropriate OS exit code.
// It is an alias of doctorcmd.ProbeStatusError so main.go and backup.go keep
// constructing and matching the same concrete type.
type ProbeStatusError = doctorcmd.ProbeStatusError

// isProbeStatusError reports whether err is a ProbeStatusError and, if so,
// sets *out to its value.
func isProbeStatusError(err error, out *ProbeStatusError) bool {
	return doctorcmd.IsProbeStatusError(err, out)
}

// exitCodeForProbeStatus returns the conventional exit code for a probe status.
func exitCodeForProbeStatus(status string) int {
	return doctorcmd.ExitCodeForProbeStatus(status)
}

// doctorCmd returns the "doctor" Cobra command with health-check subcommands.
// Exit codes:
//
//	0 = healthy or degraded (degraded is informational; check stderr for detail)
//	2 = broken
func doctorCmd() *cobra.Command {
	statusSub := doctorStatusCmd()
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Health checks for the veska runtime",
		Long:         "Health checks for the veska runtime.\n\nWith no subcommand, runs the 'status' rollup across all subsystems.",
		SilenceUsage: true,
		// solov2-jtl5.2: bare `veska doctor` now runs the status rollup
		// instead of just printing help. The rollup is what users actually
		// want as a first-call health probe; the per-subsystem probes
		// (embedder, egress, storage, …) remain explicit subcommands.
		Args: cobra.NoArgs,
		RunE: statusSub.RunE,
	}
	// Preserve --json on the parent so `veska doctor --json` behaves like
	// `veska doctor status --json`.
	cmd.Flags().AddFlagSet(statusSub.Flags())

	cmd.AddCommand(
		statusSub,
		doctorEgressCmd(),
		doctorStorageCmd(),
		doctorEmbedderCmd(),
		doctorConfigCmd(),
		doctorPostPromotionQueueCmd(),
		doctorWikiRenderCmd(),
		doctorServiceCmd(),
		doctorPipelinesCmd(),
		doctorBundleCmd(),
		doctorBackupCmd(),
		doctorResetCrashLoopCmd(),
		doctorSavingsCmd(),
	)

	return cmd
}

// doctorStatusCmd returns the "doctor status" subcommand that rolls up all probes.
func doctorStatusCmd() *cobra.Command {
	var jsonOut bool
	var verbose bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Overall health rollup across all subsystems",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunStatus(cmd.OutOrStdout(), doctorcmd.StatusOptions{JSON: jsonOut, Verbose: verbose})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "include failed queue rows inline")
	return cmd
}

// doctorSavingsCmd returns the "doctor savings" subcommand (also registered
// top-level as `veska savings`, solov2-3bu). It reads the per-search telemetry
// the daemon's MCP search handler writes and renders today / 7d / all-time
// savings bars. Logic lives in internal/cli/savingscmd; this is Cobra glue
// (solov2-0omh.8).
func doctorSavingsCmd() *cobra.Command {
	var jsonOut bool
	var aggregate bool
	cmd := &cobra.Command{
		Use:          "savings",
		Short:        "Show inline-snippet token savings per period",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return savingscmd.Run(savingscmd.Params{
				Out:         cmd.OutOrStdout(),
				VeskaHome:   config.DefaultVectorDir(),
				Now:         time.Now(),
				JSON:        jsonOut,
				Aggregate:   aggregate,
				FormatBytes: humanBytes,
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	// --aggregate forces the pooled single-bucket output. Today this is the
	// only mode (the recorder is not partitioned by repo_id yet — solov2-0ql0),
	// so the flag is effectively a no-op alias introduced now so the eventual
	// per-repo default has a stable opt-out and scripts keep working unchanged.
	cmd.Flags().BoolVar(&aggregate, "aggregate", false, "pool every registered repo into a single row (current default)")
	return cmd
}
