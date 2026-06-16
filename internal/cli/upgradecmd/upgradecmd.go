// Package upgradecmd holds the business logic behind the `veska upgrade`
// command. cmd/veska/upgrade.go is reduced to Cobra construction whose RunE
// body delegates here, following the cmd = glue / logic-in-packages pattern
// The validate -> stage -> chmod -> atomic-rename ->
// optional-restart sequence lives here; the daemon restart is injected as a
// closure so this package does not depend on the service manager.
package upgradecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNoManager is returned when --restart is requested but no service manager
// was wired in (RestartFn is nil).
var ErrNoManager = errors.New("service manager not available")

// Params bundles the inputs of Run.
type Params struct {
	// Source is the path to the new binary to install.
	Source string
	// Target is the binary path to replace; "" resolves the current executable.
	Target string
	// Restart requests a daemon restart after a successful swap.
	Restart bool
	Out     io.Writer
	// RestartFn restarts the daemon; nil means no service manager was wired,
	// so a --restart request errors with ErrNoManager.
	RestartFn func(context.Context) error
}

// Run atomically replaces the target binary with Source, then optionally
// restarts the daemon. The swap is a POSIX-atomic rename on the same
// filesystem; a failed stage is cleaned up so the target is never left
// half-written.
func Run(ctx context.Context, p Params) error {
	info, err := os.Stat(p.Source)
	if err != nil {
		return fmt.Errorf("source binary %q not found: %w", p.Source, err)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("source binary %q is not executable", p.Source)
	}

	target := p.Target
	if target == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot determine current executable: %w", err)
		}
		target = exe
	}

	if err := swapBinary(p.Source, target); err != nil {
		return err
	}
	fmt.Fprintf(p.Out, "upgraded: %s -> %s\n", p.Source, target)

	if p.Restart {
		if p.RestartFn == nil {
			return ErrNoManager
		}
		if err := p.RestartFn(ctx); err != nil {
			return fmt.Errorf("restart after upgrade: %w", err)
		}
		fmt.Fprintln(p.Out, "service restarted")
	}
	return nil
}

// swapBinary copies src to <target>.new, makes it executable, then atomically
// renames it onto target. A failed step removes the staging file.
func swapBinary(src, target string) error {
	staging := target + ".new"
	if err := copyFile(src, staging); err != nil {
		return fmt.Errorf("staging new binary: %w", err)
	}
	if err := os.Chmod(staging, 0755); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("chmod staging binary: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("rename %q -> %q: %w", staging, target, err)
	}
	return nil
}

// copyFile copies src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
