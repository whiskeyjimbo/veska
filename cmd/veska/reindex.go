// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/cli/reindexcmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// reparserFactoryFunc builds a cold-scan reparser closure from an open SQLite
// pool set and an IgnoreLoader. It is the cmd-owned seam shared by `veska
// reindex` and `veska search` (injected into reindexcmd.Params and
// searchcmd.RunOpts) so tests can substitute a spy that records invocations
// without standing up the real ingester/promoter/sinks pipeline. The seam is
// threaded through newRootCmd's options rather than a mutable package global.
type reparserFactoryFunc = func(*sqlite.Pools, application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error)

// defaultReparserFactory is the production seam: it delegates to the shared
// composition root, which wires Ingester + Promoter + PromotionStore + sinks
// and installs the CLI post-promotion check pipeline (secret-scan always,
// vuln-scan per config) exactly as the daemon's cold-scan path does.
func defaultReparserFactory(pools *sqlite.Pools, loader application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	return composition.NewCLIColdScanReparser(pools, loader)
}

// matchByPath canonicalises path with EvalSymlinks (matching the repo
// registry's stored form) and returns the registered repo whose RootPath
// equals it. An unregistered path is a typed error. It is a cmd-owned seam
// shared with `veska search` (searchcmd.RunOpts.MatchByPath) and reindexcmd.
func matchByPath(ctx context.Context, db *sql.DB, path string) (repo.Record, error) {
	canonical, err := filepath.Abs(path)
	if err != nil {
		return repo.Record{}, fmt.Errorf("reindex: abs %q: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}

	records, err := repo.List(ctx, db)
	if err != nil {
		return repo.Record{}, fmt.Errorf("reindex: list repos: %w", err)
	}
	for _, r := range records {
		if r.RootPath == canonical {
			return r, nil
		}
	}
	return repo.Record{}, fmt.Errorf("reindex: path %q is not a registered repository", canonical)
}

// reindexCmd returns the "reindex" Cobra command. It forces a full cold-scan
// reparse of the named (or cwd-resolved) repo; reindexcmd.Run owns the
// daemon-dispatch fork and the direct-SQLite fallback. The reparser factory is
// injected so tests can drive the command with a spy.
func reindexCmd(reparserFactory reparserFactoryFunc) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:          "reindex [<repo-id-or-path>]",
		Short:        "Force a full cold-scan reparse of a repository",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// MergeTarget: positional arg wins if both supplied (with a
			// stderr note), --repo is used when no positional given.
			target := reindexcmd.MergeTarget(cmd.ErrOrStderr(), args, repoFlag)
			return reindexcmd.Run(cmd.Context(), reindexcmd.Params{
				Target:          target,
				Out:             cmd.OutOrStdout(),
				ErrOut:          cmd.ErrOrStderr(),
				DaemonRunning:   daemonRunning,
				DialReindex:     reindexcmd.DefaultDial,
				ReparserFactory: reparserFactory,
				MatchByPath:     matchByPath,
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id, short_id, alias or path (alias for the positional arg)")
	return cmd
}
