package restorecmd

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/backup"
)

// daemonUp/daemonDown are injectable DaemonRunning stubs.
func daemonUp() bool   { return true }
func daemonDown() bool { return false }

func TestRunRejectsWrongModeCount(t *testing.T) {
	cases := map[string]Params{
		"none":              {},
		"path+latest":       {Path: "b.tar.gz", UseLatest: true},
		"path+premigration": {Path: "b.tar.gz", UsePreMigration: true},
		"latest+premig":     {UseLatest: true, UsePreMigration: true},
		"all three":         {Path: "b.tar.gz", UseLatest: true, UsePreMigration: true},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			p.Out = io.Discard
			// DaemonRunning must not be consulted when the mode check fails
			// first; a panic here would prove the ordering wrong.
			p.DaemonRunning = func() bool { panic("daemon check ran before mode validation") }
			err := Run(p)
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("want exactly-one-mode error, got %v", err)
			}
		})
	}
}

func TestRunRefusesWhileDaemonRunning(t *testing.T) {
	err := Run(Params{
		Path:          "b.tar.gz",
		Out:           io.Discard,
		DaemonRunning: daemonUp,
	})
	if !errors.Is(err, backup.ErrDaemonRunning) {
		t.Fatalf("want ErrDaemonRunning, got %v", err)
	}
}

func TestRunLatestSurfacesResolveDirError(t *testing.T) {
	sentinel := errors.New("resolve boom")
	err := Run(Params{
		UseLatest:      true,
		Out:            io.Discard,
		DaemonRunning:  daemonDown,
		ResolveReadDir: func() (string, error) { return "", sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped resolve error, got %v", err)
	}
}
