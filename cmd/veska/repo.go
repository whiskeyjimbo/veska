package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
)

// repoCmd returns the "repo" Cobra command with its sub-commands. Both add and
// remove prefer the running daemon's MCP socket (so they go through
// repoRegistrar.AddRepo / RemoveRepo and pick up the cold-scan + live-watch
// wiring) and fall back to a direct SQLite write when the daemon is
// unreachable. All command logic lives in internal/cli/repocmd; the bodies
// here are Cobra wiring + thin delegating calls (solov2-0omh.4).
func repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "repo",
		Short:        "Manage git repositories tracked by veska",
		SilenceUsage: true,
	}
	cmd.AddCommand(repoAddCmd())
	cmd.AddCommand(repoRemoveCmd())
	cmd.AddCommand(repoListCmd())
	cmd.AddCommand(repoPruneCmd())
	cmd.AddCommand(repoAliasCmd())
	cmd.AddCommand(repoUnaliasCmd())
	return cmd
}

// repoAliasCmd binds a user-defined human-friendly name to a repo .
func repoAliasCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "alias <name> <repo-id-or-prefix-or-alias>",
		Short: "Bind a human-friendly name to a repo",
		Long: `Bind a human-friendly name to a repo.

The new name comes FIRST, the existing repo SECOND — same order as
"git remote add <name> <url>". (Note this is the reverse of "ln -s <target>
<link>".) If the arguments look swapped, the command detects it and prints a
hint rather than failing silently.`,
		Example: `  # Alias the repo whose id starts with "a1b2" to the name "lib":
  veska repo alias lib a1b2

  # Overwrite an existing alias:
  veska repo alias lib c3d4 --force`,
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return repocmd.RunRepoAlias(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing alias bound to a different repo")
	return cmd
}

// repoUnaliasCmd removes a user-defined alias .
func repoUnaliasCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "unalias <name>",
		Short:        "Remove a user-defined alias",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return repocmd.RunRepoUnalias(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

// repoPruneCmd is the deprecated alias for `repo remove --missing`. Hidden
// from help but kept for one release so existing scripts/muscle memory keep
// working . Remove this command after one release cycle.
func repoPruneCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:          "prune",
		Short:        "Deprecated: use `veska repo remove --missing`",
		Hidden:       true,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.ErrOrStderr(), "`veska repo prune` is deprecated; use `veska repo remove --missing`")
			return repocmd.RunRepoRemoveMissing(cmd.Context(), cmd.OutOrStdout(), dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list candidates without removing them")
	return cmd
}

// repoListCmd prints every registered repo .
func repoListCmd() *cobra.Command {
	var includeExternal bool
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List registered git repositories",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return repocmd.RunRepoList(cmd.Context(), cmd.OutOrStdout(), includeExternal)
		},
	}
	cmd.Flags().BoolVar(&includeExternal, "include-external", false,
		"also show synthetic ext:<module> repos created by `veska deps index`")
	return cmd
}

// repoAddCmd registers a git repository (local path or remote URL) and installs
// hooks (solov2-kxo5.3 covers the URL form).
func repoAddCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:          "add <path-or-url>",
		Short:        "Register a git repository (local path or remote URL) and install hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := args[0]
			if repocmd.LooksLikeRepoURL(root) {
				return repocmd.RunRepoAddURL(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), root, wait)
			}
			return repocmd.RunRepoAddPath(cmd.Context(), cmd.OutOrStdout(), root, wait)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "block until the cold scan completes; print live progress")
	return cmd
}

// repoRemoveCmd unifies the deregister surface :
//   - `repo remove <id|path>` — remove one
//   - `repo remove --missing`  — remove every repo whose root dir is gone
//   - `repo remove --all`      — wipe registry (requires --yes confirmation)
//   - `--dry-run` is honored for --missing and --all.
func repoRemoveCmd() *cobra.Command {
	var (
		missing bool
		all     bool
		yes     bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:          "remove [<id-or-path>]",
		Short:        "Deregister a repository and remove hooks",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case missing && all:
				return fmt.Errorf("repo remove: --missing and --all are mutually exclusive")
			case (missing || all) && len(args) == 1:
				return fmt.Errorf("repo remove: positional argument not allowed with --missing/--all")
			case !missing && !all && len(args) == 0:
				return fmt.Errorf("repo remove: missing repo id-or-path (or pass --missing / --all)")
			}
			ctx, w := cmd.Context(), cmd.OutOrStdout()
			switch {
			case missing:
				return repocmd.RunRepoRemoveMissing(ctx, w, dryRun)
			case all:
				return repocmd.RunRepoRemoveAll(ctx, w, cmd.InOrStdin(), dryRun, yes)
			default:
				return repocmd.RunRepoRemoveOne(ctx, w, args[0])
			}
		},
	}
	cmd.Flags().BoolVar(&missing, "missing", false, "remove every repo whose root directory no longer exists")
	cmd.Flags().BoolVar(&all, "all", false, "remove every registered repo (requires --yes or interactive confirmation)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip interactive confirmation (required for --all in scripts)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be removed without changing the registry")
	return cmd
}
