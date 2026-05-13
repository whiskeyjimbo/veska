package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/crashloop"
	"github.com/whiskeyjimbo/engram/solov2/internal/logrotate"
)

const (
	// daemonLogSize is 100 MiB — rotate when the active log exceeds this.
	daemonLogSize = 100 << 20

	// daemonLogKeep is the number of rotated copies to retain.
	daemonLogKeep = 5
)

func newRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "engram-daemon",
		Short:        "Engram long-running daemon (supervises Dolt + Qdrant)",
		SilenceUsage: true,
	}
}

func main() {
	// Set up rotating log writer at ~/.engram/logs/daemon.log.
	logPath := filepath.Join(config.DefaultVectorDir(), "logs", "daemon.log")
	lw, err := logrotate.NewRotatingWriter(logPath, daemonLogSize, daemonLogKeep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engram-daemon: open log file: %v\n", err)
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
			slog.Error("crash-loop breaker tripped — run `engram doctor reset-crash-loop` to recover")
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
