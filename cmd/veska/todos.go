package main

import (
	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/findingscmd"
)

// todosCmd wraps eng_find_todos. parity wrapper. Lists the
// TODO/FIXME findings harvested by the promotion checks; repo_id auto-resolves
// from the cwd when --repo is omitted.
func todosCmd() *cobra.Command {
	var (
		repoFlag      string
		includeClosed bool
		jsonOut       bool
	)
	cmd := &cobra.Command{
		Use:          "todos",
		Short:        "List TODO/FIXME findings in the indexed source (wraps eng_find_todos)",
		Long:         "List the TODO/FIXME findings the promotion checks harvested from the indexed source. Defaults to open TODOs; pass --include-closed to also show resolved ones.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return findingscmd.RunTodos(cmd.Context(), findingscmd.TodosParams{
				RepoID:        repoFlag,
				IncludeClosed: includeClosed,
				JSONOut:       jsonOut,
				Out:           cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias (default: cwd-resolved repo)")
	cmd.Flags().BoolVar(&includeClosed, "include-closed", false, "also show closed TODO findings")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_todos shape)")
	return cmd
}
