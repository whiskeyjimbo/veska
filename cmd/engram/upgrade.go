package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// upgradeCmd returns the "upgrade" Cobra command.
// mgr may be nil; --restart is only available when mgr is non-nil.
func upgradeCmd(mgr ServiceManager) *cobra.Command {
	var target string
	var restart bool

	cmd := &cobra.Command{
		Use:          "upgrade <path>",
		Short:        "Atomically replace the engram binary with a new build",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			src := args[0]

			// 1. Validate source exists.
			info, err := os.Stat(src)
			if err != nil {
				return fmt.Errorf("source binary %q not found: %w", src, err)
			}

			// 2. Validate source is executable.
			if info.Mode()&0111 == 0 {
				return fmt.Errorf("source binary %q is not executable", src)
			}

			// 3. Resolve target (default: current executable).
			if target == "" {
				exe, err := os.Executable()
				if err != nil {
					return fmt.Errorf("cannot determine current executable: %w", err)
				}
				target = exe
			}

			// 4. Copy source to <target>.new.
			staging := target + ".new"
			if err := copyFile(src, staging); err != nil {
				return fmt.Errorf("staging new binary: %w", err)
			}

			if err := os.Chmod(staging, 0755); err != nil {
				_ = os.Remove(staging)
				return fmt.Errorf("chmod staging binary: %w", err)
			}

			// 5. Atomic rename onto target (POSIX-atomic on same filesystem).
			if err := os.Rename(staging, target); err != nil {
				_ = os.Remove(staging)
				return fmt.Errorf("rename %q -> %q: %w", staging, target, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "upgraded: %s -> %s\n", src, target)

			// 6. Optional restart.
			if restart {
				if mgr == nil {
					return errNoManager
				}
				if err := mgr.Restart(cmd.Context()); err != nil {
					return fmt.Errorf("restart after upgrade: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "service restarted")
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "", "binary path to replace (default: current executable)")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart the daemon service after swapping the binary")

	return cmd
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
