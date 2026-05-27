package main

import (
	"bufio"
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

// initFlags carries the boolean choices initCmd resolves before calling
// runInit — separates flag-handling from the core flow and keeps runInit
// testable without spinning up cobra.
type initFlags struct {
	yes    bool // --yes: auto-accept all prompts with the default answer.
	noVuln bool // --no-vuln: force vuln_source disabled, skip the prompt.
	stdin  io.Reader
	// interactive reports whether stdin is a TTY. Non-interactive callers
	// (CI, agent harnesses, install pipelines) get the default answer
	// silently — the prompt is suppressed entirely and the chosen default
	// is echoed in the summary so the caller can tell what happened
	// (solov2-mgyy).
	interactive bool
}

// runInit performs the full first-run initialisation flow:
//  1. Creates the ~/.veska/ directory layout (logs/, cache/, state/).
//  2. Resolves the embedder via the same boot-election as the daemon. The
//     default (model2vec/static) is in-process and needs no external service,
//     so init never fails for lack of Ollama. Only an explicit
//     VESKA_EMBEDDER=ollama probes Ollama and hard-fails when it is unhealthy.
//  3. Prompts to enable [vuln_source] (solov2-pvyo) unless --yes / --no-vuln
//     short-circuits.
//  4. Prints a short summary to out on success.
func runInit(ctx context.Context, deps initDeps, flags initFlags, out io.Writer) error {
	// ── 1. Create directory layout ───────────────────────────────────────────
	for _, sub := range []string{"logs", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(deps.veskaHome, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	// solov2-pvyo: resolve vuln_source choice BEFORE writing the config so
	// we write it in its final shape (uncommented when enabled). Defaults
	// to Y so `veska init -y` opts the user in — junior-journey UX choice,
	// the scanner ships behind a single feature flag and is safe to enable.
	vulnEnabled, err := resolveVulnChoice(flags, out)
	if err != nil {
		return err
	}

	// solov2-w1ng: CONFIG-SURFACE.md promises `veska init` writes
	// ~/.veska/config.toml when absent. Honour that — drop a starter file
	// so a junior can grep, edit, restart, and go. Never overwrites an
	// existing file (the prompt above does NOT mutate an existing config).
	if err := writeDefaultConfigIfAbsent(deps.veskaHome, vulnEnabled); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	// ── 2. Embedder ──────────────────────────────────────────────────────────
	embedderLine, tip, err := resolveInitEmbedder(ctx, deps)
	if err != nil {
		return err
	}

	// ── 3. Summary ───────────────────────────────────────────────────────────
	fmt.Fprintln(out, "veska initialized")
	fmt.Fprintf(out, "data:     %s  (override with VESKA_HOME)\n", deps.veskaHome)
	fmt.Fprintf(out, "backups:  %s  (co-located under VESKA_HOME; a single `rm -rf %s` clears all state; solov2-n57f)\n", defaultBackupDirHint(), deps.veskaHome)
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
	// solov2-0cv6: surface the first-five-minutes walkthrough right at
	// init so a junior never has to grep --help for the next step. The
	// three-command block is the minimum to get from 'veska init' to
	// 'veska search' producing real results.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "next steps:")
	fmt.Fprintln(out, "  1. veska service install && veska service start  # run the indexer daemon")
	fmt.Fprintln(out, "  2. veska repo add <path> --wait                  # index your first repo")
	fmt.Fprintln(out, "  3. veska search \"your question\"                  # semantic search the graph")
	fmt.Fprintln(out, "  see also: veska init --agent claude|cursor|...   # MCP setup for editors")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "ready")

	return nil
}

// defaultBackupDirHint returns the user-visible default backup directory
// path. It mirrors config.DefaultBackupDir so init's summary lines up with
// what `veska backup create` will actually write to (solov2-n57f).
func defaultBackupDirHint() string {
	return config.DefaultBackupDir()
}

// stdinIsInteractive reports whether os.Stdin is a TTY. Used to decide
// whether to prompt or silently take the default during `veska init`
// (solov2-mgyy). On any stat error we conservatively report false — the
// quiet, non-interactive default behaviour is the right answer when the
// shape of stdin can't be determined.
func stdinIsInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// resolveVulnChoice asks the user whether to enable OSV vulnerability scanning
// at init time (solov2-pvyo). Non-interactive paths short-circuit:
//   - --no-vuln → always disabled.
//   - --yes (or stdin missing/closed) → accept the default (enabled).
//   - existing config.toml on disk → skip the prompt entirely; we never
//     mutate an existing file.
func resolveVulnChoice(flags initFlags, out io.Writer) (bool, error) {
	if flags.noVuln {
		return false, nil
	}
	if flags.yes || flags.stdin == nil {
		return true, nil
	}
	// solov2-mgyy: when stdin isn't a TTY (CI, piped install, agent
	// harness) skip the prompt entirely and take the default. Echo what
	// we chose so the caller can read the summary and know vuln scanning
	// is on without inspecting config.toml.
	if !flags.interactive {
		fmt.Fprintln(out, "OSV vulnerability scanning: enabled (default; non-interactive stdin)")
		return true, nil
	}
	fmt.Fprint(out, "Enable OSV vulnerability scanning? [Y/n] ")
	reader := bufio.NewReader(flags.stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF without input — treat as default (yes). Avoids breaking
		// callers that wire init into a non-tty pipeline. Echo what we
		// chose so a user piping/redirecting stdin (which presents as a
		// TTY to the parent shell) still sees the resolved choice
		// instead of an unanswered prompt (solov2-cgut).
		fmt.Fprintln(out, "yes (stdin EOF; accepting default)")
		return true, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		// Unknown answer — be conservative and don't enable. Print what
		// we interpreted so the junior knows why scanning is off.
		fmt.Fprintf(out, "  (answer %q not understood; leaving vuln_source disabled)\n", strings.TrimSpace(line))
		return false, nil
	}
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
		return "(local CPU, low-quality fallback)"
	}
	name := modelID
	if i := strings.Index(modelID, "("); i >= 0 {
		if j := strings.LastIndex(modelID, ")"); j > i {
			name = modelID[i+1 : j]
		}
	}
	if p, err := model2vec.TryLoad(veskaHome, name); err == nil && p != nil {
		return "(local CPU, downloaded model)"
	}
	if _, ok := model2vec.Embedded(); ok {
		return "(local CPU, model baked into the binary; no Ollama required — set VESKA_EMBEDDER=ollama to switch)"
	}
	return "(local CPU)"
}

// configTemplateHeader prefixes every generated config.toml. CONFIG-SURFACE.md
// is the canonical full surface; this file is the starter sketch.
const configTemplateHeader = `# Veska daemon config.
# Written by ` + "`veska init`" + ` when this file is absent.
# Full surface: docs/operations/CONFIG-SURFACE.md.
#
# Uncomment a block (or edit a value) and restart
# (` + "`veska service restart`" + `) to apply.

`

// vulnSourceBlockEnabled is the live (uncommented) [vuln_source] block —
// written when init resolves the prompt to "yes" (solov2-pvyo).
const vulnSourceBlockEnabled = `# OSV.dev vulnerability scanner. After re-indexing existing repos
# (` + "`veska reindex <path>`" + `), findings appear in ` + "`veska findings list`" + `.
[vuln_source]
provider         = "osv"
refresh_interval = "24h"
`

// vulnSourceBlockDisabled is the commented-out variant. It still appears so
// a junior who later wants OSV can grep + uncomment without reading the
// CONFIG-SURFACE doc.
const vulnSourceBlockDisabled = `# OSV.dev vulnerability scanner (off; opt-in).
# After enabling, run ` + "`veska reindex <path>`" + ` to scan
# already-promoted repos.
# [vuln_source]
# provider         = "osv"
# refresh_interval = "24h"
`

// writeDefaultConfigIfAbsent writes the starter config.toml only when the
// file does not already exist. vulnEnabled selects whether the
// [vuln_source] block is written live or commented out (solov2-pvyo).
// Idempotent on re-init — never overwrites an existing config.
func writeDefaultConfigIfAbsent(veskaHome string, vulnEnabled bool) error {
	path := filepath.Join(veskaHome, "config.toml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	body := configTemplateHeader
	if vulnEnabled {
		body += vulnSourceBlockEnabled
	} else {
		body += vulnSourceBlockDisabled
	}
	return os.WriteFile(path, []byte(body), 0o644)
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
	var noVuln bool
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
			flags := initFlags{
				yes:         yes,
				noVuln:      noVuln,
				stdin:       cmd.InOrStdin(),
				interactive: stdinIsInteractive(),
			}
			return runInit(cmd.Context(), deps, flags, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "auto-accept all prompts (non-interactive mode)")
	cmd.Flags().BoolVar(&noVuln, "no-vuln", false, "skip the OSV vulnerability-scanner prompt and leave it disabled (solov2-pvyo)")
	cmd.Flags().StringVar(&agent, "agent", "",
		"write a per-agent instruction snippet to the current project ("+
			strings.Join(supportedFlavorNames(), ", ")+")")
	cmd.Flags().BoolVar(&updateGitignore, "update-gitignore", false,
		"with --agent: also write a veska-managed block to .gitignore covering generated artifacts (solov2-zm6i; off by default)")
	return cmd
}
