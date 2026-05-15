package main

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

	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/crashloop"
	"github.com/whiskeyjimbo/veska/internal/logrotate"
)

const (
	// daemonLogSize is 100 MiB — rotate when the active log exceeds this.
	daemonLogSize = 100 << 20

	// daemonLogKeep is the number of rotated copies to retain.
	daemonLogKeep = 5
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "veska-daemon",
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

func main() {
	// Set up rotating log writer at ~/.veska/logs/daemon.log.
	logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
	lw, err := logrotate.NewRotatingWriter(logPath, daemonLogSize, daemonLogKeep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "veska-daemon: open log file: %v\n", err)
		os.Exit(1)
	}
	defer lw.Close()

	// Redirect structured logs to the rotating writer.
	slog.SetDefault(slog.New(slog.NewJSONHandler(lw, nil)))

	// SIGHUP: reopen the log file (supports external log rotation).
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

	// Crash-loop guard: refuse to start if the breaker has tripped.
	if err := crashloop.Check(config.DefaultVectorDir()); err != nil {
		if errors.Is(err, crashloop.ErrBroken) {
			slog.Error("crash-loop breaker tripped — run `veska doctor reset-crash-loop` to recover")
			os.Exit(crashloop.ExitCode)
		}
		slog.Error("crash-loop check failed", "err", err)
		os.Exit(1)
	}

	if err := newRootCmd().Execute(); err != nil {
		slog.Error("daemon command failed", "err", err)
		os.Exit(1)
	}
}
