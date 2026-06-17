// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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
// calls into that package.

// ProbeStatusError is returned by doctor subcommands when a probe yields a
// non-healthy status. main translates it to the appropriate OS exit code.
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
		// bare `veska doctor` now runs the status rollup
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
		doctorIdentityCmd(),
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
// top-level as `veska savings`). It reads the per-search telemetry
// the daemon's MCP search handler writes and renders today / 7d / all-time
// savings bars. Logic lives in internal/cli/savingscmd; this is Cobra glue.
func doctorSavingsCmd() *cobra.Command {
	var jsonOut bool
	var aggregate bool
	cmd := &cobra.Command{
		Use:   "savings",
		Short: "Show inline-snippet token savings per period",
		Long: "Show inline-snippet token savings per period (today / 7d / all-time).\n\n" +
			"\"Savings\" is the ratio 1 - snippet_chars/file_chars: how much agent-side\n" +
			"file-read traffic the inline snippets in eng_search_semantic results saved.\n\n" +
			"Warmup: a period reads \"warming up\" until it has recorded at least 20\n" +
			"eng_search_semantic calls. Below that the sample is too small to be\n" +
			"meaningful - a single short snippet can drive the ratio negative - so only\n" +
			"the running call count is shown, not a percentage. Once a period crosses\n" +
			"20 calls its row switches to a percentage.\n\n" +
			"The counter only advances on eng_search_semantic searches (not symbol/\n" +
			"context lookups), and those come from the MCP server the daemon runs. Until\n" +
			"an MCP-aware editor is wired up - or eng_search_semantic is called directly\n" +
			"several times - savings stays in warmup.",
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
	// aggregate forces the pooled single-bucket output. The default breaks
	// savings down per repo; --aggregate sums every repo into
	// one "all repos" bucket for the headline number or for scripts that parsed
	// the pre-breakdown shape.
	cmd.Flags().BoolVar(&aggregate, "aggregate", false, "pool every repo into a single bucket instead of the per-repo breakdown")
	return cmd
}
