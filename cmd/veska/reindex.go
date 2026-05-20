package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/config"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// reparserFactory builds a cold-scan reparser closure from an open SQLite
// pool set and an IgnoreLoader. It is a package-level seam so tests can
// substitute a spy that records invocations without spinning the real
// ingester/promoter/sinks pipeline up.
//
// Production path: defaultReparserFactory — wires Ingester + Promoter +
// PromotionStore + sinks (FTSSink, EmbedRefSink) exactly as
// cmd/veska-daemon/wire.go does. The duplication is intentional and mirrors
// the cmd/veska/wiki.go pattern; see follow-up bead noted via bd remember
// for the eventual extraction of a shared helper.
var reparserFactory = defaultReparserFactory

func defaultReparserFactory(pools *sqlite.Pools, loader application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	staging := application.NewStagingArea()
	gate := application.NewIngestionGate(staging)
	parser := treesitter.NewGoParser()
	ingester := application.NewIngester(parser, staging, gate)

	// Same sink set the daemon registers: FTSSink + EmbedRefSink. Missing
	// EmbedRefSink would silently break AC2 ("re-enqueued for embedding"),
	// so this set must stay in lock-step with cmd/veska-daemon/wire.go.
	promotionStore := sqlite.NewPromotionStore(
		pools.WriteHot,
		[]sqlite.PromotionSink{
			sqlite.NewFTSSink(),
			sqlite.NewEmbedRefSink(),
		},
	)
	promoter := application.NewPromoter(staging, promotionStore)

	reparser, err := application.NewColdScanReparser(
		ingester, promoter, gitwatch.Querier{},
		application.WithIgnoreLoader(loader),
	)
	if err != nil {
		return nil, fmt.Errorf("reindex: build cold-scan reparser: %w", err)
	}
	return reparser, nil
}

// reindexCmd returns the "reindex" Cobra command. It runs a full cold-scan
// reparse of the named (or cwd-resolved) repo unconditionally — bypassing
// the daemon's StartupResync gate that skips at-HEAD repos. This is the
// on-demand path that repopulates nodes.snippet and re-enqueues every
// promoted node for embedding against the live +snippet projection.
//
// The CLI opens the SQLite pools itself; if the daemon is running and
// holds a busy write transaction, this command will block on the write
// lock until the daemon releases it (sqlite WAL allows concurrent reads
// but one writer at a time).
func reindexCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "reindex [<repo-id-or-path>]",
		Short:        "Force a full cold-scan reparse of a repository",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			ctx := cmd.Context()

			dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
			// Apply migrations so the schema is present even on a freshly
			// created database; mirror the wiki command's belt-and-braces
			// open-twice pattern.
			if _, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{}); err != nil {
				return fmt.Errorf("reindex: migrate sqlite: %w", err)
			}
			pools, err := sqlite.OpenPools(dbPath)
			if err != nil {
				return fmt.Errorf("reindex: open sqlite pools: %w", err)
			}
			defer func() { _ = pools.Close() }()

			var target string
			if len(args) == 1 {
				target = args[0]
			}
			rec, err := resolveReindexTarget(ctx, pools.ReadDB, target)
			if err != nil {
				return err
			}

			loader := func(repoRoot string) (application.IgnoreMatcher, error) {
				return fsignore.Load(repoRoot)
			}
			reparser, err := reparserFactory(pools, loader)
			if err != nil {
				return err
			}

			appRec := application.RepoRecord{
				RepoID:          rec.RepoID,
				RootPath:        rec.RootPath,
				ActiveBranch:    rec.ActiveBranch,
				LastPromotedSHA: rec.LastPromotedSHA,
			}
			if appRec.ActiveBranch == "" {
				appRec.ActiveBranch = "main"
			}

			fmt.Fprintf(w, "reindexing %s at %s...\n", appRec.RepoID, appRec.RootPath)
			if err := reparser(ctx, appRec); err != nil {
				return fmt.Errorf("reindex: %w", err)
			}

			// Best-effort HEAD lookup for the trailing message; an error
			// here does not invalidate the reindex itself.
			head, _ := gitwatch.Querier{}.HEAD(appRec.RootPath)
			fmt.Fprintf(w, "reindex complete: repo %s at SHA %s\n", appRec.RepoID, head)
			return nil
		},
	}
}

// resolveReindexTarget picks the repo to reindex. With no arg, the cwd is
// canonicalised and matched against every registered repo's RootPath; with
// an arg, it is treated first as a repo id (repo.Get) and, on miss, as a
// path (canonicalised + matched against List).
func resolveReindexTarget(ctx context.Context, db *sql.DB, target string) (repo.Record, error) {
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return repo.Record{}, fmt.Errorf("reindex: getwd: %w", err)
		}
		return matchByPath(ctx, db, cwd)
	}

	// Try as id first.
	if rec, err := repo.Get(ctx, db, target); err != nil {
		return repo.Record{}, fmt.Errorf("reindex: get repo: %w", err)
	} else if rec.RepoID != "" {
		return rec, nil
	}

	// Then as a filesystem path.
	if _, statErr := os.Stat(target); statErr == nil {
		return matchByPath(ctx, db, target)
	}
	return repo.Record{}, fmt.Errorf("reindex: repo %q is not registered (not a known id and not a registered path)", target)
}

// matchByPath canonicalises path with EvalSymlinks (matching the repo
// registry's stored form) and returns the registered repo whose RootPath
// equals it. An unregistered path is a typed error.
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
