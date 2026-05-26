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
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
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

	// solov2-w1ng: CONFIG-SURFACE.md promises `veska init` writes
	// ~/.veska/config.toml when absent. Honour that — drop a commented
	// starter file so a junior can grep, uncomment, restart, and go.
	// Never overwrites an existing file.
	if err := writeDefaultConfigIfAbsent(deps.veskaHome); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
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
		// solov2-sft7: make this LOUD. The quiet 'tip:' line buried under
		// 'ready' meant junior users routinely shipped with the low-quality
		// static-v2 embedder.
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  WARNING: booting on the low-quality static-v2 embedder fallback.")
		fmt.Fprintln(out, "    Semantic search quality will be noticeably degraded.")
		fmt.Fprintln(out, "    Fix: run `veska install model2vec` (one-time ~62MB download),")
		fmt.Fprintln(out, "    or rebuild with `make build` (default fat binary).")
		fmt.Fprintln(out)
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
	line = prov.ModelID() + " " + embedderProvenance(deps.veskaHome, prov.ModelID())
	if prov.ModelID() == embedstatic.ModelID {
		tip = "tip: run 'veska install model2vec' for higher-quality code search"
	}
	return line, tip, nil
}

// embedderProvenance reports where the elected provider's weights came from,
// so `veska init` can disambiguate fat (compiled in), downloaded (~/.veska),
// and static-v2 fallback (solov2-veci). The model name is extracted from
// ModelID — model2vec providers render as "model2vec(<name>)".
func embedderProvenance(veskaHome, modelID string) string {
	if modelID == embedstatic.ModelID {
		return "(in-process, fallback)"
	}
	name := modelID
	if i := strings.Index(modelID, "("); i >= 0 {
		if j := strings.LastIndex(modelID, ")"); j > i {
			name = modelID[i+1 : j]
		}
	}
	if p, err := model2vec.TryLoad(veskaHome, name); err == nil && p != nil {
		return "(in-process, downloaded)"
	}
	if _, ok := model2vec.Embedded(); ok {
		return "(in-process, fat)"
	}
	return "(in-process)"
}

// defaultConfigTemplate is the file written by `veska init` when
// ~/.veska/config.toml is absent. Every section is commented so the
// daemon keeps using its built-in defaults; uncommenting a block is
// the affordance for enabling it. Keep this short — CONFIG-SURFACE.md
// is the canonical reference.
const defaultConfigTemplate = `# Veska daemon config.
# Written by ` + "`veska init`" + ` when this file is absent.
# Full surface: docs/operations/CONFIG-SURFACE.md.
#
# Every block below is commented out — the daemon falls back to its
# built-in defaults for anything missing. Uncomment a block and
# restart (` + "`veska service restart`" + `) to apply.

# OSV.dev vulnerability scanner (off by default; opt-in).
# After enabling, run ` + "`veska reindex <path>`" + ` to scan
# already-promoted repos.
# [vuln_source]
# provider         = "osv"
# refresh_interval = "24h"
`

// writeDefaultConfigIfAbsent writes defaultConfigTemplate to
// ~/.veska/config.toml only if it does not already exist. Idempotent on
// re-init.
func writeDefaultConfigIfAbsent(veskaHome string) error {
	path := filepath.Join(veskaHome, "config.toml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigTemplate), 0o644)
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
	var updateGitignore bool

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
				return writeAgentSnippet(cwd, agent, cmd.OutOrStdout(), updateGitignore)
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
	cmd.Flags().BoolVar(&updateGitignore, "update-gitignore", false,
		"with --agent: also write a veska-managed block to .gitignore covering generated artifacts (solov2-zm6i; off by default)")
	return cmd
}
