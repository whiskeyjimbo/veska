package main

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/engram/solov2/internal/repo"
)

// repoCmd returns the "repo" Cobra command with "add" and "remove" sub-commands.
// db may be nil when the database is not yet available; the sub-commands handle
// that case by printing an error message and exiting with status 1.
func repoCmd(db *sql.DB) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "repo",
		Short:        "Manage git repositories tracked by engram",
		SilenceUsage: true,
	}
	cmd.AddCommand(repoAddCmd(db))
	cmd.AddCommand(repoRemoveCmd(db))
	return cmd
}

func repoAddCmd(db *sql.DB) *cobra.Command {
	return &cobra.Command{
		Use:          "add <path>",
		Short:        "Register a git repository and install hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if db == nil {
				fmt.Fprintln(os.Stderr, "database not available")
				os.Exit(1)
			}
			id, err := repo.Add(cmd.Context(), db, args[0])
			if err != nil {
				return fmt.Errorf("repo add: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added repo %s\n", id)
			return nil
		},
	}
}

func repoRemoveCmd(db *sql.DB) *cobra.Command {
	return &cobra.Command{
		Use:          "remove <id>",
		Short:        "Deregister a repository and remove hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if db == nil {
				fmt.Fprintln(os.Stderr, "database not available")
				os.Exit(1)
			}
			if err := repo.Remove(cmd.Context(), db, args[0]); err != nil {
				return fmt.Errorf("repo remove: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "removed")
			return nil
		},
	}
}
