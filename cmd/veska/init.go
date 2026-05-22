package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/embedderprobe"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
)

// initDeps holds injectable dependencies for runInit, enabling testing without
// real filesystem side-effects or network calls.
type initDeps struct {
	veskaHome string
	// override is the VESKA_EMBEDDER value; "" (auto) and "model2vec"/"static"
	// resolve in-process and never touch the network. Only "ollama" probes.
	override string
	probe    func(ctx context.Context, url, model string) (*embedderprobe.ProbeResult, error)
	goos     string
}

// runInit performs the full first-run initialisation flow:
//  1. Creates the ~/.veska/ directory layout (logs/, cache/, state/).
//  2. Resolves the embedder via the same boot-election as the daemon. The
//     default (model2vec/static) is in-process and needs no external service,
//     so init never fails for lack of Ollama. Only an explicit
//     VESKA_EMBEDDER=ollama probes Ollama and hard-fails when it is unhealthy.
//  3. Prints a short summary to out on success.
func runInit(ctx context.Context, deps initDeps, yes bool, out io.Writer) error {
	// ── 1. Create directory layout ───────────────────────────────────────────
	for _, sub := range []string{"logs", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(deps.veskaHome, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	// ── 2. Embedder ──────────────────────────────────────────────────────────
	embedderLine, tip, err := resolveInitEmbedder(ctx, deps)
	if err != nil {
		return err
	}

	// ── 3. Summary ───────────────────────────────────────────────────────────
	fmt.Fprintln(out, "veska initialized")
	fmt.Fprintf(out, "data:     %s\n", deps.veskaHome)
	fmt.Fprintf(out, "embedder: %s\n", embedderLine)
	fmt.Fprintln(out, "service:  not installed (run: veska service install)")
	fmt.Fprintln(out, "repo:     not added (run: veska repo add <path>)")
	if tip != "" {
		fmt.Fprintln(out, tip)
	}
	fmt.Fprintln(out, "ready")

	return nil
}

// resolveInitEmbedder reports the embedder init will use and an optional tip.
// It mirrors the daemon's election: in-process for the default path (no
// network), Ollama probe + hard-fail only when explicitly overridden.
func resolveInitEmbedder(ctx context.Context, deps initDeps) (line, tip string, err error) {
	if strings.EqualFold(deps.override, elect.OverrideOllama) {
		url := envOrDefault("VESKA_OLLAMA_URL", defaultOllamaURL)
		model := envOrDefault("VESKA_EMBED_MODEL", defaultModelName)
		result, perr := deps.probe(ctx, url, model)
		if perr != nil {
			return "", "", fmt.Errorf("embedder probe failed: %w", perr)
		}
		if result.Status != "healthy" {
			hint := embedderprobe.InstallHint(deps.goos, model)
			return "", "", fmt.Errorf("embedder not healthy (%s): %s", result.Status, hint)
		}
		return fmt.Sprintf("ollama %s @ %s (%s)", model, url, result.Status), "", nil
	}

	prov, rerr := elect.Resolve(elect.Config{VeskaHome: deps.veskaHome, Override: deps.override})
	if rerr != nil {
		return "", "", fmt.Errorf("embedder election: %w", rerr)
	}
	line = prov.ModelID() + " (in-process)"
	if prov.ModelID() == embedstatic.ModelID {
		tip = "tip: run 'veska install model2vec' for higher-quality code search"
	}
	return line, tip, nil
}

// envOrDefault returns the env var when non-empty, else def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// initCmd returns the "init" Cobra command that runs the first-run flow.
func initCmd() *cobra.Command {
	var yes bool
	var agent string

	cmd := &cobra.Command{
		Use:          "init",
		Short:        "First-run setup, or write per-agent instruction snippet with --agent",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --agent is project-scoped and short-circuits the
			// machine-scoped first-run flow: the two intentionally
			// don't co-execute (solov2-m81).
			if agent != "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("init --agent: cwd: %w", err)
				}
				return writeAgentSnippet(cwd, agent, cmd.OutOrStdout())
			}
			deps := initDeps{
				veskaHome: config.DefaultVectorDir(),
				override:  os.Getenv("VESKA_EMBEDDER"),
				probe:     embedderprobe.Probe,
				goos:      runtime.GOOS,
			}
			return runInit(cmd.Context(), deps, yes, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "auto-accept all prompts (non-interactive mode)")
	cmd.Flags().StringVar(&agent, "agent", "",
		"write a per-agent instruction snippet to the current project ("+
			strings.Join(supportedFlavorNames(), ", ")+")")
	return cmd
}
