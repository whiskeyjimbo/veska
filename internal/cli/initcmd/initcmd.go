// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package initcmd holds the business logic behind `veska init`: the first-run
// machine setup (directory layout, starter config.toml, embedder election) and
// the project-scoped per-agent instruction snippet writer (--agent). cmd/veska/
// init.go is reduced to Cobra command construction whose RunE delegates here,
// following the cmd = glue / logic-in-packages pattern established by
// reindexcmd, symbolcmd, graphcmd, and findingscmd.
package initcmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/cli/doctorcmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/embedderprobe"
)

// In-process embedder defaults, shared with the doctor probes.
const (
	defaultOllamaURL = doctorcmd.DefaultOllamaURL
	defaultModelName = doctorcmd.DefaultModelName
)

// Deps holds injectable dependencies for Run, enabling testing without real
// filesystem side-effects or network calls.
type Deps struct {
	VeskaHome string
	// Override is the VESKA_EMBEDDER value; "" (auto) and "model2vec"/"static"
	// resolve in-process and never touch the network. Only "ollama" probes.
	Override string
	Probe    func(ctx context.Context, url, model string) (*embedderprobe.ProbeResult, error)
	GOOS     string
}

// Flags carries the boolean choices the init command resolves before calling
// Run - separates flag-handling from the core flow and keeps Run testable
// without spinning up cobra.
type Flags struct {
	Yes    bool // yes: auto-accept all prompts with the default answer.
	NoVuln bool // no-vuln: force vuln_source disabled, skip the prompt.
	Stdin  io.Reader
	// Interactive reports whether stdin is a TTY. Non-interactive callers
	// (CI, agent harnesses, install pipelines) get the default answer
	// silently - the prompt is suppressed entirely and the chosen default
	// is echoed in the summary so the caller can tell what happened
	Interactive bool
}

// Run performs the full first-run initialisation flow:
//  1. Creates the ~/.veska/ directory layout (logs/, cache/, state/).
//  2. Resolves the embedder via the same boot-election as the daemon. The
//     default (model2vec/static) is in-process and needs no external service,
//     so init never fails for lack of Ollama. Only an explicit
//     VESKA_EMBEDDER=ollama probes Ollama and hard-fails when it is unhealthy.
//  3. Prompts to enable [vuln_source] unless --yes / --no-vuln
//     short-circuits.
//  4. Prints a short summary to out on success.
func Run(ctx context.Context, deps Deps, flags Flags, out io.Writer) error {
	// ── 1. Create directory layout ───────────────────────────────────────────
	for _, sub := range []string{"logs", "cache", "state"} {
		if err := os.MkdirAll(filepath.Join(deps.VeskaHome, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", sub, err)
		}
	}

	// resolve vuln_source choice BEFORE writing the config so
	// we write it in its final shape (uncommented when enabled). Defaults
	// to Y so `veska init -y` opts the user in - junior-journey UX choice,
	// the scanner ships behind a single feature flag and is safe to enable.
	vulnEnabled, err := ResolveVulnChoice(flags, out)
	if err != nil {
		return err
	}

	// CONFIG-SURFACE.md promises `veska init` writes
	// ~/.veska/config.toml when absent. Honour that - drop a starter file
	// so a junior can grep, edit, restart, and go. Never overwrites an
	// existing file (the prompt above does NOT mutate an existing config).
	if err := writeDefaultConfigIfAbsent(deps.VeskaHome, vulnEnabled); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	// ── 2. Embedder ──────────────────────────────────────────────────────────
	embedderLine, tip, err := resolveInitEmbedder(ctx, deps)
	if err != nil {
		return err
	}

	// ── 3. Summary ───────────────────────────────────────────────────────────
	fmt.Fprintln(out, "veska initialized")
	fmt.Fprintf(out, "data:     %s  (override with VESKA_HOME)\n", deps.VeskaHome)
	fmt.Fprintf(out, "backups:  %s  (co-located under VESKA_HOME; a single `rm -rf %s` clears all state)\n", config.DefaultBackupDir(), deps.VeskaHome)
	fmt.Fprintf(out, "embedder: %s\n", embedderLine)
	fmt.Fprintln(out, "service:  not installed (run: veska service install)")
	fmt.Fprintln(out, "repo:     not added (run: veska repo add <path>)")
	// surface the vuln-scan choice in the summary so
	// `init -y` users don't get OSV egress silently enabled.
	if vulnEnabled {
		fmt.Fprintln(out, "vuln:     OSV scanner enabled (auto-runs on every repo promotion; results land in `veska findings list`; rerun init with --no-vuln to disable)")
	} else {
		fmt.Fprintln(out, "vuln:     OSV scanner disabled (no network egress for vuln scans)")
	}
	if tip != "" {
		// make this LOUD. The quiet 'tip:' line buried under
		// 'ready' meant junior users routinely shipped with the low-quality
		// static-v2 embedder.
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  WARNING: booting on the low-quality static-v2 embedder fallback.")
		fmt.Fprintln(out, "    Semantic search quality will be noticeably degraded.")
		fmt.Fprintln(out, "    Fix: run `veska install model2vec` (one-time ~62MB download),")
		fmt.Fprintln(out, "    or rebuild with `make build` (default fat binary).")
		fmt.Fprintln(out)
	}
	// surface the first-five-minutes walkthrough right at
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
	// tiny discoverability hints. New users hit two
	// papercuts on first contact: invoking bin/veska by absolute path
	// (no PATH suggestion anywhere), and discovering -y / --yes only by
	// reading source when scripting init from CI/Docker.
	fmt.Fprintln(out, "tip: copy or symlink bin/veska to a directory on PATH (e.g. ~/.local/bin/) so you can run `veska` directly.")
	fmt.Fprintln(out, "tip: for non-interactive use (CI, Docker, agent harnesses) rerun with `-y` (alias --yes) to accept all defaults.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "ready")

	return nil
}

// StdinIsInteractive reports whether os.Stdin is a TTY. Used to decide
// whether to prompt or silently take the default during `veska init`
// On any stat error we conservatively report false - the
// quiet, non-interactive default behaviour is the right answer when the
// shape of stdin can't be determined.
func StdinIsInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// ResolveVulnChoice asks the user whether to enable OSV vulnerability scanning
// at init time. Non-interactive paths short-circuit:
//
//	no-vuln → always disabled.
//	yes (or stdin missing/closed) → accept the default (enabled).
//	existing config.toml on disk → skip the prompt entirely; we never
//	  mutate an existing file.
func ResolveVulnChoice(flags Flags, out io.Writer) (bool, error) {
	if flags.NoVuln {
		return false, nil
	}
	if flags.Yes || flags.Stdin == nil {
		return true, nil
	}
	// when stdin isn't a TTY (CI, piped install, agent
	// harness) skip the prompt entirely and take the default. Echo what
	// we chose so the caller can read the summary and know vuln scanning
	// is on without inspecting config.toml.
	if !flags.Interactive {
		fmt.Fprintln(out, "OSV vulnerability scanning: enabled (default; non-interactive stdin - pass --no-vuln to opt out, --yes to silence this line)")
		return true, nil
	}
	// surface that the default makes the daemon query osv.dev
	// over the network, and point at --no-vuln / --yes so a junior never
	// has to grep --help to script the install.
	// Peek for an immediate EOF before printing anything - some agent
	// harnesses present a TTY-ish stdin that's already closed, in which
	// case the prompt + "stdin EOF" parenthetical reads as an error
	// Skip straight to the non-interactive line so the
	// output is identical to the !flags.Interactive branch.
	reader := bufio.NewReader(flags.Stdin)
	if _, peekErr := reader.Peek(1); peekErr != nil {
		fmt.Fprintln(out, "OSV vulnerability scanning: enabled (default; stdin closed - pass --no-vuln to opt out, --yes to silence this line)")
		return true, nil
	}
	fmt.Fprintln(out, "Enable OSV vulnerability scanning? (queries osv.dev over the network)")
	fmt.Fprint(out, "  [Y/n] (or rerun with --yes / --no-vuln to skip this prompt): ")
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		// EOF after the prompt was printed (rare: the peek above caught
		// the common case). Echo a clean default-accepted line so the
		// user sees the resolved choice instead of an unanswered prompt.
		fmt.Fprintln(out, "yes (default; pass --no-vuln next time to opt out)")
		return true, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		// Unknown answer - be conservative and don't enable. Print what
		// we interpreted so the junior knows why scanning is off.
		fmt.Fprintf(out, "  (answer %q not understood; leaving vuln_source disabled)\n", strings.TrimSpace(line))
		return false, nil
	}
}

// resolveInitEmbedder reports the embedder init will use and an optional tip.
// It mirrors the daemon's election: in-process for the default path (no
// network), Ollama probe + hard-fail only when explicitly overridden.
func resolveInitEmbedder(ctx context.Context, deps Deps) (line, tip string, err error) {
	if strings.EqualFold(deps.Override, elect.OverrideOllama) {
		url := envOrDefault("VESKA_OLLAMA_URL", defaultOllamaURL)
		model := envOrDefault("VESKA_EMBED_MODEL", defaultModelName)
		result, perr := deps.Probe(ctx, url, model)
		if perr != nil {
			return "", "", fmt.Errorf("embedder probe failed: %w", perr)
		}
		if result.Status != "healthy" {
			hint := embedderprobe.InstallHint(deps.GOOS, model)
			return "", "", fmt.Errorf("embedder not healthy (%s): %s", result.Status, hint)
		}
		return fmt.Sprintf("ollama %s @ %s (%s)", model, url, result.Status), "", nil
	}

	prov, rerr := elect.Resolve(elect.Config{VeskaHome: deps.VeskaHome, Override: deps.Override})
	if rerr != nil {
		return "", "", fmt.Errorf("embedder election: %w", rerr)
	}
	line = prov.ModelID() + " " + embedderProvenance(deps.VeskaHome, prov.ModelID())
	if prov.ModelID() == embedstatic.ModelID {
		tip = "tip: run 'veska install model2vec' for higher-quality code search"
	}
	return line, tip, nil
}

// embedderProvenance reports where the elected provider's weights came from,
// so `veska init` can disambiguate fat (compiled in), downloaded (~/.veska),
// and static-v2 fallback. The model name is extracted from
// ModelID - model2vec providers render as "model2vec(<name>)".
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
		return "(local CPU, model baked into the binary; no Ollama required - set VESKA_EMBEDDER=ollama to switch)"
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

// vulnSourceBlockEnabled is the live (uncommented) [vuln_source] block
// written when init resolves the prompt to "yes".
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
// [vuln_source] block is written live or commented out.
// Idempotent on re-init - never overwrites an existing config.
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
