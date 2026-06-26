// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/symbolcmd"
	mcpinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// The symbol/context command logic lives in internal/cli/symbolcmd; the
// constructors below are Cobra glue whose RunE bodies are thin delegating
// calls into that package. The shared cwd→repo resolver
// helpers (resolveRepoFromCWD/autoResolveRepo) live in shared.go since they
// are used across the deps and findings families. The symbol/context family
// deliberately does not auto-scope to the cwd's repo; it lives
// alongside them only as Cobra glue.

// symbolCmd wraps eng_find_symbol so users can drive the same lookup their
// editor would, without typing the JSON-RPC envelope. repo_id auto-resolves
// when exactly one repo is registered (the daemon's
// resolveRepoIDOrSingleton); pass --repo to scope across multiple
func symbolCmd() *cobra.Command {
	var (
		repoFlag string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "symbol <name>",
		Short: "Look up symbols by name (wraps eng_find_symbol)",
		// Long reuses the MCP DescFindSymbolMatching fragment so the
		// unqualified-match / exact-first rule can't drift from the
		// eng_find_symbol description.
		Long: "Find symbols by unqualified name or symbol path.\n\n" +
			"Auto-resolves repo_id from the only registered repo when --repo is omitted; " +
			"pass --repo <short_id> to scope across multiple repos.\n\n" +
			mcpinfra.DescFindSymbolMatching,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		// when --repo is omitted, do NOT auto-scope to the cwd's
		// repo - let the daemon fan out across every registered repo.
		RunE: func(cmd *cobra.Command, args []string) error {
			return symbolcmd.RunFind(cmd.Context(), symbolcmd.FindParams{
				Symbol:  args[0],
				RepoID:  repoFlag,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
				ErrOut:  cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_symbol shape)")
	return cmd
}

// contextCmd wraps eng_get_context_pack so users can pull the same
// caller+callee+test bundle the agent would, without crafting JSON
func contextCmd() *cobra.Command {
	var (
		repoFlag   string
		jsonOut    bool
		symbolFlag string
		scopeFlag  string
	)
	cmd := &cobra.Command{
		Use:   "context <symbol>",
		Short: "Bundle a symbol with its callers/callees/tests (wraps eng_get_context_pack)",
		// Long reuses the MCP DescContextPack fragment (shared purpose +
		// cross-repo behavior, both true for the CLI) so it can't drift from
		// the eng_get_context_pack description. The MCP-only anchor prose
		// (node_id/task_id) is intentionally NOT shared - `veska context`
		// takes only a symbol.
		Long: mcpinfra.DescContextPack,
		// accept the symbol as either a positional arg or a
		// symbol flag. The MCP tool's JSON param is "symbol" so users
		// naturally try --symbol; reject only when both or neither are
		// supplied.
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sym, err := pickSymbolArg(args, symbolFlag)
			if err != nil {
				return err
			}
			return symbolcmd.RunContext(cmd.Context(), symbolcmd.ContextParams{
				Symbol:  sym,
				RepoID:  repoFlag,
				Scope:   scopeFlag,
				JSONOut: jsonOut,
				Out:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id or short_id (default: the sole registered repo)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_get_context_pack shape)")
	cmd.Flags().StringVar(&symbolFlag, "symbol", "", "symbol name (alternative to the positional arg)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "neighborhood width: 'focused' (seed + direct callees) or 'full' (default)")
	return cmd
}

// pickSymbolArg resolves the symbol from the positional arg or the --symbol
// flag, rejecting the both-set and neither-set cases.
func pickSymbolArg(args []string, symbolFlag string) (string, error) {
	switch {
	case len(args) == 1 && symbolFlag == "":
		return args[0], nil
	case len(args) == 0 && symbolFlag != "":
		return symbolFlag, nil
	case len(args) == 1 && symbolFlag != "":
		return "", fmt.Errorf("context: pass symbol as positional arg OR --symbol, not both")
	default:
		return "", fmt.Errorf("context: a symbol is required (positional or --symbol)")
	}
}
