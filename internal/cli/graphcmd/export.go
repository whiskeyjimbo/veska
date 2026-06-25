// SPDX-License-Identifier: AGPL-3.0-only

package graphcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/graphexport"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// ExportParams bundles the inputs of RunExport.
type ExportParams struct {
	// OutPath is the destination snapshot file. Required.
	OutPath string
	// RepoArg is the optional --repo selector (id, short_id, or alias). When
	// empty the repo is resolved from cwd, or the sole registered repo.
	RepoArg string
	// Branch optionally overrides the exported branch. Empty exports the
	// repo's registered active branch.
	Branch string
	Out    io.Writer
}

// RunExport implements `veska graph export <out.json>`. It opens the local
// graph DB directly (no daemon required - WAL reads coexist with a running
// daemon), exports a deterministic snapshot of the resolved repo, and writes
// it to OutPath. The snapshot is the shareable contract consumed by
// `veska graph serve`.
func RunExport(ctx context.Context, p ExportParams) error {
	if p.OutPath == "" {
		return fmt.Errorf("graph export: output path is required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("graph export: open graph db: %w", err)
	}
	defer func() { _ = pools.Close() }()

	rec, err := resolveExportRepo(ctx, pools, p.RepoArg)
	if err != nil {
		return err
	}
	branch := p.Branch
	if branch == "" {
		branch = rec.ActiveBranch
	}
	if branch == "" {
		branch = "main"
	}

	svc, err := composition.NewGraphExportService(pools, staging.NewArea())
	if err != nil {
		return fmt.Errorf("graph export: %w", err)
	}
	snap, err := svc.Export(ctx, rec.RepoID, branch, rec.RootPath)
	if err != nil {
		return fmt.Errorf("graph export: %w", err)
	}
	data, err := graphexport.Marshal(snap)
	if err != nil {
		return fmt.Errorf("graph export: %w", err)
	}
	if err := os.WriteFile(p.OutPath, data, 0o644); err != nil {
		return fmt.Errorf("graph export: write %s: %w", p.OutPath, err)
	}
	fmt.Fprintf(p.Out, "wrote %s (%d nodes, %d edges) for repo %s\n",
		p.OutPath, len(snap.Nodes), len(snap.Edges), repocmd.ShortRepoID(rec.RepoID))
	return nil
}

// resolveExportRepo selects the repo to export without contacting the daemon:
// an explicit --repo selector wins; otherwise the sole registered repo is
// used, falling back to matching the caller's cwd against registered repo
// roots. An ambiguous or empty result returns an actionable error naming the
// --repo flag.
func resolveExportRepo(ctx context.Context, pools *sqlite.Pools, repoArg string) (repo.Record, error) {
	recs, err := repo.List(ctx, pools.ReadDB)
	if err != nil {
		return repo.Record{}, fmt.Errorf("graph export: list repos: %w", err)
	}
	if len(recs) == 0 {
		return repo.Record{}, fmt.Errorf("graph export: no repos registered; run `veska repo add <path>` first")
	}
	if repoArg != "" {
		rec, err := repocmd.ResolveCLIRepoID(recs, repoArg)
		if err != nil {
			return repo.Record{}, fmt.Errorf("graph export: %w", err)
		}
		return rec, nil
	}
	if len(recs) == 1 {
		return recs[0], nil
	}
	if rec, ok := repoForCWD(recs); ok {
		return rec, nil
	}
	return repo.Record{}, fmt.Errorf("graph export: multiple repos registered and cwd matches none; pass --repo <id>")
}

// repoForCWD returns the registered repo whose root contains the caller's cwd.
func repoForCWD(recs []repo.Record) (repo.Record, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return repo.Record{}, false
	}
	return repoContaining(recs, cwd)
}

// repoContaining returns the registered repo whose root contains cwd,
// preferring the longest (most specific) matching root when repos nest.
func repoContaining(recs []repo.Record, cwd string) (repo.Record, bool) {
	var best repo.Record
	found := false
	for _, r := range recs {
		if r.RootPath == "" {
			continue
		}
		if cwd == r.RootPath || strings.HasPrefix(cwd, r.RootPath+string(os.PathSeparator)) {
			if !found || len(r.RootPath) > len(best.RootPath) {
				best = r
				found = true
			}
		}
	}
	return best, found
}
