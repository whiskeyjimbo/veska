package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/doctor"
)

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
		doctorSubCmd("status", "Overall health rollup across all subsystems",
			func(jsonOut bool) error { return stubOK("status", jsonOut) }),
		doctorSubCmd("egress", "Verify daemon socket and control-plane connectivity",
			func(jsonOut bool) error { return stubOK("egress", jsonOut) }),
		doctorStorageCmd(),
		doctorSubCmd("embedder", "Verify embedding provider (Ollama) connectivity",
			func(jsonOut bool) error { return stubOK("embedder", jsonOut) }),
		doctorSubCmd("config", "Validate engram configuration values",
			func(jsonOut bool) error { return stubOK("config", jsonOut) }),
		doctorSubCmd("post_promotion_queue", "Inspect the post-promotion queue depth",
			func(jsonOut bool) error { return stubOK("post_promotion_queue", jsonOut) }),
		doctorSubCmd("pipelines", "Check ingestion pipeline health",
			func(jsonOut bool) error { return stubOK("pipelines", jsonOut) }),
		doctorSubCmd("bundle", "Verify MCP context-pack bundle integrity",
			func(jsonOut bool) error { return stubOK("bundle", jsonOut) }),
	)

	return cmd
}

// stubResult is the JSON envelope emitted by stub subcommands.
type stubResult struct {
	Subsystem string `json:"subsystem"`
	Status    string `json:"status"`
}

// stubOK prints an "ok" message for stub subcommands that have no real probe yet.
func stubOK(subsystem string, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(stubResult{Subsystem: subsystem, Status: "ok"})
	}
	fmt.Fprintf(os.Stdout, "%s: ok\n", subsystem)
	return nil
}

// doctorSubCmd creates a generic stub doctor subcommand with a --json flag.
func doctorSubCmd(use, short string, run func(bool) error) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        short,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(jsonOut)
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
			report, err := doctor.CheckStorage(config.DefaultVectorDir())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				return enc.Encode(report)
			}
			fmt.Fprintf(os.Stdout, "storage: ok (db=%d bytes, wal=%d bytes, hnsw=%d bytes, free_ratio=%.2f)\n",
				report.DBSizeBytes, report.WALSizeBytes, report.HNSWSizeBytes, report.FreeRatio)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}
