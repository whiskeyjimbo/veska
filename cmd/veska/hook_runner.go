// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/hookcmd"
)

// The git-hook shim logic lives in internal/cli/hookcmd; the constructors
// below are Cobra glue whose RunE bodies delegate into that package
// Both hooks always succeed so git never blocks a commit or
// checkout on a best-effort index notification.

// hookRunnerCmd returns the "hook-runner" Cobra command with sub-commands.
func hookRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "hook-runner",
		Short:        "Git hook shims installed by veska init",
		SilenceUsage: true,
	}
	cmd.AddCommand(postCommitCmd())
	cmd.AddCommand(postCheckoutCmd())
	return cmd
}

// postCommitCmd returns the "hook-runner post-commit" sub-command.
func postCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "post-commit",
		Short:        "Notify daemon after a git commit (installed by veska init)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return hookcmd.RunPostCommit()
		},
	}
}

// postCheckoutCmd returns the "hook-runner post-checkout" sub-command.
func postCheckoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "post-checkout",
		Short:        "Update active branch after a git checkout (installed by veska init)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return hookcmd.RunPostCheckout()
		},
	}
}
