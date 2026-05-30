package main

import (
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/doctorcmd"
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

// In-process embedder defaults, also consumed by init.go.
const (
	defaultOllamaURL = doctorcmd.DefaultOllamaURL
	defaultModelName = doctorcmd.DefaultModelName
)

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
			return doctorcmd.RunStatus(cmd.OutOrStdout(), jsonOut, verbose)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "include failed queue rows inline")
	return cmd
}

// doctorPostPromotionQueueCmd returns the "doctor post_promotion_queue" subcommand.
func doctorPostPromotionQueueCmd() *cobra.Command {
	var (
		jsonOut      bool
		purgeOrphans bool
	)
	cmd := &cobra.Command{
		Use:          "post_promotion_queue",
		Short:        "Inspect the post-promotion queue depth and failed rows",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunPostPromotionQueue(cmd.OutOrStdout(), jsonOut, purgeOrphans)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	cmd.Flags().BoolVar(&purgeOrphans, "purge-orphans", false, "delete failed rows whose repo_id is no longer registered")
	return cmd
}

// doctorWikiRenderCmd returns the "doctor wiki_render" subcommand.
func doctorWikiRenderCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "wiki_render",
		Short:        "Report the age of the last successful wiki render",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunWikiRender(cmd.Context(), cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorPipelinesCmd returns the "doctor pipelines" subcommand.
func doctorPipelinesCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "pipelines",
		Short:        "Report review-pipeline token usage against the configured caps",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunPipelines(cmd.Context(), cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorEmbedderCmd returns the "doctor embedder" subcommand. It verifies the
// embedder the daemon actually elected — in-process by default, Ollama only
// when VESKA_EMBEDDER=ollama.
func doctorEmbedderCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "embedder",
		Short:        "Verify the elected embedding provider",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunEmbedder(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorEgressCmd returns the "doctor egress" subcommand.
func doctorEgressCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "egress",
		Short:        "Verify daemon socket and control-plane connectivity",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunEgress(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorConfigCmd returns the "doctor config" subcommand.
func doctorConfigCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Validate veska configuration values",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunConfig(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorServiceCmd returns the "doctor service" subcommand.
// Exit codes follow SOLO-13 §2.1:
//
//	0 = healthy  (daemon running, no broken marker)
//	1 = degraded (daemon unreachable, no broken marker)
//	2 = broken   (broken marker present)
func doctorServiceCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "service",
		Short:        "Check supervisor state and broken-marker presence",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunService(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorBackupCmd returns the "doctor backup" subcommand.
// Exit codes follow SOLO-13 §2.1:
//
//	0 = healthy  (most recent .tar.gz exists and passes gzip verification)
//	1 = degraded (no backup files found)
//	2 = broken   (most recent backup fails gzip verification)
func doctorBackupCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "backup",
		Short:        "Verify most recent backup archive and report its age",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunBackup(cmd.OutOrStdout(), jsonOut, hasBackupTarballs)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorResetCrashLoopCmd returns the "doctor reset-crash-loop" subcommand that
// removes the broken marker and crash-count files from the veska home directory,
// allowing the daemon to start after a crash-loop trip.
func doctorResetCrashLoopCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "reset-crash-loop",
		Short:        "Clear broken marker and restart counter so the daemon can start again",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunResetCrashLoop(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorStorageCmd returns the "doctor storage" subcommand.
func doctorStorageCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "storage",
		Short:        "Report filesystem storage metrics for the veska data directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunStorage(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorBundleCmd returns the "doctor bundle" subcommand that writes a diagnostic
// tarball (manifest, all probe outputs, redacted audit tail) to a temp directory
// and prints the resulting path.
func doctorBundleCmd() *cobra.Command {
	var outputDir string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "bundle",
		Short:        "Write a diagnostic tarball with all probe outputs and audit tail",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doctorcmd.RunBundle(cmd.OutOrStdout(), jsonOut, outputDir)
		},
	}
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the tarball (default: system temp dir)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}
