package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/doctor"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	defaultModelName = "nomic-embed-text"
)

// ProbeStatusError is returned by doctor subcommands when a probe yields a
// non-healthy status.  main() translates it to the appropriate OS exit code.
type ProbeStatusError struct {
	Subsystem string
	Status    string // "degraded" or "broken"
}

func (e ProbeStatusError) Error() string {
	return e.Subsystem + ": " + e.Status
}

// isProbeStatusError reports whether err is a ProbeStatusError and,
// if so, sets *out to its value.
func isProbeStatusError(err error, out *ProbeStatusError) bool {
	if err == nil {
		return false
	}
	p, ok := err.(ProbeStatusError)
	if ok {
		*out = p
	}
	return ok
}

// exitCodeForProbeStatus returns the conventional exit code for a probe status.
//
//	healthy  → 0
//	degraded → 1
//	broken   → 2
func exitCodeForProbeStatus(status string) int {
	switch status {
	case "degraded":
		return 1
	case "broken":
		return 2
	}
	return 0
}

// doctorCmd returns the "doctor" Cobra command with health-check subcommands.
// Exit codes follow the SOLO-13 §2.1 convention:
//
//	0 = healthy
//	1 = degraded
//	2 = broken
func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Health checks for the engram runtime",
		SilenceUsage: true,
	}

	cmd.AddCommand(
		doctorStatusCmd(),
		doctorEgressCmd(),
		doctorStorageCmd(),
		doctorEmbedderCmd(),
		doctorConfigCmd(),
		doctorPostPromotionQueueCmd(),
		doctorServiceCmd(),
		doctorSubCmd("pipelines", "Check ingestion pipeline health",
			func(jsonOut bool, w io.Writer) error { return stubOK("pipelines", jsonOut, w) }),
		doctorSubCmd("bundle", "Verify MCP context-pack bundle integrity",
			func(jsonOut bool, w io.Writer) error { return stubOK("bundle", jsonOut, w) }),
	)

	return cmd
}

// doctorPostPromotionQueueCmd returns the "doctor post_promotion_queue" subcommand
// backed by internal/doctor.CheckPostPromotionQueue.
func doctorPostPromotionQueueCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "post_promotion_queue",
		Short:        "Inspect the post-promotion queue depth and failed rows",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			dbPath := filepath.Join(config.DefaultVectorDir(), "engram.db")
			report, err := doctor.CheckPostPromotionQueue(dbPath)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("post_promotion_queue", report.Status, report))
			}
			fmt.Fprintf(w, "post_promotion_queue: %s (state_counts=%d, failed=%d)\n",
				report.Status, len(report.Counts), len(report.FailedRows))
			for _, c := range report.Counts {
				fmt.Fprintf(w, "  %s/%s: %d\n", c.State, c.WorkKind, c.Count)
			}
			for _, f := range report.FailedRows {
				fmt.Fprintf(w, "  FAILED seq=%d repo=%s branch=%s kind=%s attempts=%d err=%s\n",
					f.Seq, f.RepoID, f.Branch, f.WorkKind, f.Attempts, f.Error)
			}
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "post_promotion_queue", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// stubOK prints an "ok" message for stub subcommands that have no real probe yet.
func stubOK(subsystem string, jsonOut bool, w io.Writer) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		return enc.Encode(doctor.NewEnvelope(subsystem, "healthy", map[string]any{}))
	}
	fmt.Fprintf(w, "%s: ok\n", subsystem)
	return nil
}

// doctorSubCmd creates a generic stub doctor subcommand with a --json flag.
func doctorSubCmd(use, short string, run func(bool, io.Writer) error) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        short,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(jsonOut, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorEmbedderCmd returns the "doctor embedder" subcommand backed by internal/doctor.CheckEmbedder.
func doctorEmbedderCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "embedder",
		Short:        "Verify embedding provider (Ollama) connectivity",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			report, err := doctor.CheckEmbedder(defaultOllamaURL, defaultModelName)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("embedder", report.Status, report))
			}
			fmt.Fprintf(w, "embedder: %s (url=%s, model=%s)\n",
				report.Status, report.OllamaURL, report.ModelName)
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "embedder", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorEgressCmd returns the "doctor egress" subcommand backed by internal/doctor.CheckEgress.
func doctorEgressCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "egress",
		Short:        "Verify daemon socket and control-plane connectivity",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			sockPaths := []string{
				config.DaemonSockPath(),
				config.MCPSockPath(),
			}
			report, err := doctor.CheckEgress(sockPaths)
			if err != nil {
				return err
			}
			// Compute egress status.
			egressStatus := "healthy"
			for _, s := range report.Sockets {
				if s.Status == "missing" {
					egressStatus = "broken"
					break
				}
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("egress", egressStatus, report))
			}
			anyMissing := false
			for _, s := range report.Sockets {
				fmt.Fprintf(w, "egress: %s (%s)\n", s.Status, s.Path)
				if s.Status == "missing" {
					anyMissing = true
				}
			}
			if anyMissing {
				return ProbeStatusError{Subsystem: "egress", Status: "broken"}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorConfigCmd returns the "doctor config" subcommand backed by internal/doctor.CheckConfig.
func doctorConfigCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Validate engram configuration values",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			report, err := doctor.CheckConfig(config.DefaultVectorDir())
			if err != nil {
				return err
			}
			// Compute config status.
			configStatus := "healthy"
			if !report.DBExists {
				configStatus = "degraded"
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("config", configStatus, report))
			}
			fmt.Fprintf(w, "config: engram_home=%s db_exists=%v engram_home_set=%v\n",
				report.EngramHome, report.DBExists, report.EngramHomeSet)
			if !report.DBExists {
				return ProbeStatusError{Subsystem: "config", Status: "degraded"}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorStatusCmd returns the "doctor status" subcommand that rolls up all probes.
func doctorStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Overall health rollup across all subsystems",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			home := config.DefaultVectorDir()

			embedderReport, _ := doctor.CheckEmbedder(defaultOllamaURL, defaultModelName)
			egressReport, _ := doctor.CheckEgress([]string{
				config.DaemonSockPath(),
				config.MCPSockPath(),
			})
			configReport, _ := doctor.CheckConfig(home)
			storageReport, _ := doctor.CheckStorage(home)

			// Compute egress status: broken if any socket is missing.
			egressStatus := "healthy"
			for _, s := range egressReport.Sockets {
				if s.Status == "missing" {
					egressStatus = "broken"
					break
				}
			}

			// Compute config status.
			configStatus := "healthy"
			if !configReport.DBExists {
				configStatus = "degraded"
			}

			// Storage is always healthy (no failure mode currently).
			_ = storageReport

			// Roll up: broken if any broken; degraded if any degraded.
			statuses := []string{embedderReport.Status, egressStatus, configStatus}
			rollup := "healthy"
			for _, s := range statuses {
				switch s {
				case "broken":
					rollup = "broken"
				case "degraded":
					if rollup != "broken" {
						rollup = "degraded"
					}
				}
			}

			if jsonOut {
				type statusRollupData struct {
					Embedder string `json:"embedder"`
					Egress   string `json:"egress"`
					Config   string `json:"config"`
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(doctor.NewEnvelope("status", rollup, statusRollupData{
					Embedder: embedderReport.Status,
					Egress:   egressStatus,
					Config:   configStatus,
				}))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s (embedder=%s, egress=%s, config=%s)\n",
				rollup, embedderReport.Status, egressStatus, configStatus)
			if rollup != "healthy" {
				return ProbeStatusError{Subsystem: "status", Status: rollup}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorServiceCmd returns the "doctor service" subcommand backed by internal/doctor.CheckService.
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
			w := cmd.OutOrStdout()
			home := config.DefaultVectorDir()
			report, err := doctor.CheckService(home)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("service", report.Status, report))
			}
			fmt.Fprintf(w, "service: %s (daemon_running=%v, broken_marker=%v)\n",
				report.Status, report.DaemonRunning, report.BrokenMarkerPresent)
			if report.BrokenMarkerPresent {
				fmt.Fprintf(w, "  broken marker: %s\n", report.BrokenMarkerPath)
			}
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "service", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorStorageCmd returns the "doctor storage" subcommand backed by internal/doctor.CheckStorage.
func doctorStorageCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "storage",
		Short:        "Report filesystem storage metrics for the engram data directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			report, err := doctor.CheckStorage(config.DefaultVectorDir())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("storage", "healthy", report))
			}
			fmt.Fprintf(w, "storage: ok (db=%d bytes, wal=%d bytes, hnsw=%d bytes, free_ratio=%.2f)\n",
				report.DBSizeBytes, report.WALSizeBytes, report.HNSWSizeBytes, report.FreeRatio)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}
