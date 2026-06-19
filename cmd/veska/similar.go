// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/similarcmd"
)

// similarCmd wraps eng_search_similar. parity wrapper. The
// selector is a symbol name or node_id, routed the same way `veska calls`/
// `veska blast` route theirs.
func similarCmd() *cobra.Command {
	var (
		repoFlag string
		k        int
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "similar <symbol-or-node-id>",
		Short:        "Find symbols nearest to a seed in vector space (wraps eng_search_similar)",
		Long:         "Vector-nearest-neighbor search seeded by an existing symbol or node_id - 'what else looks like this?'. Use to find variants, near-duplicates, or refactor targets. The seed itself is excluded from results.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return similarcmd.RunSimilar(cmd.Context(), similarcmd.SimilarParams{
				Selector: args[0],
				RepoID:   repoFlag,
				K:        k,
				JSONOut:  jsonOut,
				Out:      cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().IntVar(&k, "k", 0, "neighbor count (0 = daemon default of 10)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_search_similar shape)")
	return cmd
}

// relatedCmd wraps eng_find_related. parity wrapper. The anchor is
// a file:line; the daemon resolves the smallest enclosing node and runs the
// same neighborhood search as `veska similar`.
func relatedCmd() *cobra.Command {
	var (
		repoFlag string
		k        int
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:          "related <file:line>",
		Short:        "Find symbols similar to the code at a file:line (wraps eng_find_related)",
		Long:         "Find symbols semantically similar to the code at a (file, line) anchor - a moat-pivot from a search hit, error trace, or editor cursor. Line is 1-indexed.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, line, err := parseFileLine(args[0])
			if err != nil {
				return err
			}
			return similarcmd.RunRelated(cmd.Context(), similarcmd.RelatedParams{
				FilePath: path,
				Line:     line,
				RepoID:   repoFlag,
				K:        k,
				JSONOut:  jsonOut,
				Out:      cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, or alias")
	cmd.Flags().IntVar(&k, "k", 0, "neighbor count (0 = daemon default of 10)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON (eng_find_related shape)")
	return cmd
}

// parseFileLine splits a "path/to/file.go:42" anchor into its path and
// 1-indexed line. Windows-style drive letters aren't a concern (the tool is
// POSIX-only), so the line is taken from the final colon-separated field.
func parseFileLine(arg string) (string, int, error) {
	idx := strings.LastIndex(arg, ":")
	if idx <= 0 || idx == len(arg)-1 {
		return "", 0, fmt.Errorf("related: anchor must be <file>:<line>, e.g. internal/foo.go:42")
	}
	line, err := strconv.Atoi(arg[idx+1:])
	if err != nil || line < 1 {
		return "", 0, fmt.Errorf("related: line must be a positive integer, got %q", arg[idx+1:])
	}
	return arg[:idx], line, nil
}
