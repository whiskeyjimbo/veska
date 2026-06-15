package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/daemon"
	"github.com/whiskeyjimbo/veska/internal/cli/mcp"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/service"
)

// rootOptions carries the injectable seams newRootCmd wires into its
// subcommands. Today only the cold-scan reparser factory (shared by `reindex`
// and `search`) is injectable; production uses defaultReparserFactory and
// tests pass a spy via withReparserFactory rather than mutating a package
// global.
type rootOptions struct {
	reparserFactory reparserFactoryFunc
}

// rootOption configures newRootCmd.
type rootOption func(*rootOptions)

// withReparserFactory overrides the cold-scan reparser factory (test seam).
func withReparserFactory(f reparserFactoryFunc) rootOption {
	return func(o *rootOptions) { o.reparserFactory = f }
}

func newRootCmd(opts ...rootOption) *cobra.Command {
	cfg := rootOptions{reparserFactory: defaultReparserFactory}
	for _, o := range opts {
		o(&cfg)
	}
	root := &cobra.Command{
		Use:   "veska",
		Short: "Veska code intelligence CLI",
		// solov2-izh6.19: surface a 'start here' hint above the alphabetised
		// command list. Cobra renders Long at the top of --help, so this
		// lands just before "Usage:" / "Available Commands:" and gives a
		// brand-new user a single obvious next step instead of 30 commands
		// to scan.
		Long: "Veska code intelligence CLI.\n\n" +
			"New here? → run \"veska init\" to set up (use `-y` for non-interactive runs).",
		Version:       shortVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Compact one-liner for `veska --version`; `veska version` keeps the
	// full multi-line build info dump .
	root.SetVersionTemplate("veska {{.Version}}\n")
	root.AddCommand(initCmd())
	root.AddCommand(hookRunnerCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(backupCmd())
	root.AddCommand(restoreCmd())
	root.AddCommand(repoCmd())
	root.AddCommand(wikiCmd())
	root.AddCommand(reindexCmd(cfg.reparserFactory))
	root.AddCommand(searchCmd(cfg.reparserFactory))
	root.AddCommand(symbolCmd())
	root.AddCommand(contextCmd())
	root.AddCommand(callsCmd())
	root.AddCommand(blastCmd())
	root.AddCommand(changedCmd())
	root.AddCommand(nodeCmd())
	root.AddCommand(fileNodesCmd())
	root.AddCommand(similarCmd())
	root.AddCommand(relatedCmd())
	root.AddCommand(clonesCmd())
	root.AddCommand(diffGateCmd())
	root.AddCommand(ownerCmd())
	root.AddCommand(todosCmd())
	root.AddCommand(entryPointsCmd())
	root.AddCommand(hotZonesCmd())
	root.AddCommand(findingsCmd())
	root.AddCommand(depsCmd())
	root.AddCommand(installCmd())
	root.AddCommand(versionCmd())
	// Top-level alias for `veska doctor savings` so the marketing-y
	// shortcut works without the doctor prefix .
	root.AddCommand(doctorSavingsCmd())
	// Hidden docs generator for the manual's CLI reference (make docs-gen).
	root.AddCommand(gendocsCmd())

	// Resolve the daemon binary path at startup. os.Executable returns the path
	// of the current binary; the daemon is reachable via the veska-daemon
	// symlink that ships alongside it .
	var mgr, dryMgr service.Manager
	if exe, err := os.Executable(); err == nil {
		// Construction errors are deliberately dropped: a nil manager is a
		// valid state every service subcommand guards against (errNoManager),
		// so there is nothing actionable to surface at startup.
		mgr, _ = service.New(exe+"-daemon", config.DefaultVectorDir())
		dryMgr, _ = service.NewDryRun(exe+"-daemon", config.DefaultVectorDir())
	}
	root.AddCommand(serviceCmd(mgr, dryMgr))
	root.AddCommand(configCmd(mgr))
	root.AddCommand(upgradeCmd(mgr))

	// `veska daemon …` / `veska mcp …` mirror the symlinked-binary entry
	// points . The symlinks remain the canonical invocation
	// for service managers and editor MCP configs.
	root.AddCommand(daemon.NewCmd())
	root.AddCommand(mcp.NewCmd())
	return root
}

func main() {
	// argv[0] dispatcher: when invoked through the veska-daemon or veska-mcp
	// symlink, route directly into the appropriate package's Run so the
	// command behaves identically to the pre-consolidation binaries.
	switch filepath.Base(os.Args[0]) {
	case "veska-daemon":
		os.Exit(daemon.Run())
	case "veska-mcp":
		os.Exit(mcp.Run())
	}

	// solov2-izh6.9: silence the default slog handler for CLI runs.
	// Sub-commands that drive in-process application code (e.g.
	// `veska search --repo <url>` doing an ephemeral cold scan) would
	// otherwise dump structured-log INFO lines from the ingester /
	// promoter / embedder onto stderr alongside the actual results. The
	// daemon binary configures its own JSON handler in daemon.Run, so
	// this only affects the CLI process.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A probe-status error carries its own conventional exit code; every
		// other error exits 1.
		var pse ProbeStatusError
		if isProbeStatusError(err, &pse) {
			os.Exit(exitCodeForProbeStatus(pse.Status))
		}
		os.Exit(1)
	}
}
