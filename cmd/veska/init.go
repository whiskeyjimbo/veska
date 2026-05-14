package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/embedderprobe"
)

// initDeps holds injectable dependencies for runInit, enabling testing without
// real filesystem side-effects or network calls.
type initDeps struct {
	veskaHome string
	probe     func(ctx context.Context, url, model string) (*embedderprobe.ProbeResult, error)
	goos      string
}

// runInit performs the full first-run initialisation flow:
//  1. Creates the ~/.veska/ directory layout (logs/, cache/, state/).
//  2. Probes the embedder; returns a non-nil error (containing the install hint)
//     when Ollama is unreachable or the embedder is not healthy (in --yes mode).
//  3. Prints a 6-line summary to out on success.
func runInit(ctx context.Context, deps initDeps, yes bool, out io.Writer) error {
	// ── 1. Create directory layout ───────────────────────────────────────────
	for _, sub := range []string{"logs", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(deps.veskaHome, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	// ── 2. Embedder probe ────────────────────────────────────────────────────
	result, err := deps.probe(ctx, defaultOllamaURL, defaultModelName)
	if err != nil {
		return fmt.Errorf("embedder probe failed: %w", err)
	}

	if result.Status != "healthy" {
		hint := embedderprobe.InstallHint(deps.goos, defaultModelName)
		return fmt.Errorf("embedder not healthy (%s): %s", result.Status, hint)
	}

	// ── 3. 6-line summary ────────────────────────────────────────────────────
	fmt.Fprintln(out, "veska initialized")
	fmt.Fprintf(out, "data:     %s\n", deps.veskaHome)
	fmt.Fprintf(out, "embedder: %s (%s @ %s)\n", result.Status, defaultModelName, defaultOllamaURL)
	fmt.Fprintln(out, "service:  not installed (run: veska service install)")
	fmt.Fprintln(out, "repo:     not added (run: veska workspace add .)")
	fmt.Fprintln(out, "ready")

	return nil
}

// initCmd returns the "init" Cobra command that runs the first-run flow.
func initCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:          "init",
		Short:        "First-run setup: create ~/.veska/ layout, probe embedder, and print summary",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps := initDeps{
				veskaHome: config.DefaultVectorDir(),
				probe:     embedderprobe.Probe,
				goos:      runtime.GOOS,
			}
			return runInit(cmd.Context(), deps, yes, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "auto-accept all prompts (non-interactive mode)")
	return cmd
}
