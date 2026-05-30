package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/crashloop"
	"github.com/whiskeyjimbo/veska/internal/platform/logrotate"
)

const (
	// daemonLogSize is 100 MiB — rotate when the active log exceeds this.
	daemonLogSize = 100 << 20

	// daemonLogKeep is the number of rotated copies to retain.
	daemonLogKeep = 5
)

// NewCmd returns the daemon cobra command, suitable for AddCommand under the
// veska root or for direct Execute via Run.
func NewCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "daemon",
		Short:        "Veska long-running daemon (MCP server + ingester + workers)",
		SilenceUsage: true,
		RunE:         runStart,
	}
	root.AddCommand(newStartCmd())
	return root
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "start",
		Short:        "Start the daemon (default action when no subcommand is given)",
		SilenceUsage: true,
		RunE:         runStart,
	}
}

// runStart constructs the Daemon from environment-defaulted Config, starts
// the long-running goroutines, and blocks until SIGINT/SIGTERM.
func runStart(cmd *cobra.Command, _ []string) error {
	d, err := newDaemon(Config{})
	if err != nil {
		return fmt.Errorf("veska-daemon: compose: %w", err)
	}

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		_ = d.Stop()
		return fmt.Errorf("veska-daemon: start: %w", err)
	}
	slog.Info("veska-daemon: running (Ctrl-C to stop)")

	<-ctx.Done()
	slog.Info("veska-daemon: shutting down")
	return d.Stop()
}

// Run is the entry point used when the binary is invoked as veska-daemon
// (via the argv[0] dispatcher in cmd/veska/main.go). It sets up log
// rotation, the SIGHUP reopen handler, and the crash-loop breaker before
// dispatching to NewCmd.
func Run() int {
	logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
	lw, err := logrotate.NewRotatingWriter(logPath, daemonLogSize, daemonLogKeep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "veska-daemon: open log file: %v\n", err)
		return 1
	}
	defer lw.Close()

	slog.SetDefault(slog.New(slog.NewJSONHandler(lw, nil)))

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		for range ch {
			if err := lw.Reopen(); err != nil {
				slog.Error("logrotate: reopen failed", "err", err)
			} else {
				slog.Info("logrotate: log file reopened")
			}
		}
	}()

	if err := crashloop.Check(config.DefaultVectorDir()); err != nil {
		if errors.Is(err, crashloop.ErrBroken) {
			slog.Error("crash-loop breaker tripped — run `veska doctor reset-crash-loop` to recover")
			return crashloop.ExitCode
		}
		slog.Error("crash-loop check failed", "err", err)
		return 1
	}

	cmd := NewCmd()
	cmd.Use = "veska-daemon"
	if err := cmd.Execute(); err != nil {
		slog.Error("daemon command failed", "err", err)
		return 1
	}
	return 0
}
