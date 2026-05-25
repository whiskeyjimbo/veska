package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// Compile-time interface assertions: the three admin-tool collaborators must
// satisfy the contracts RegisterAdminTools expects.
var (
	_ application.RepoLister = (*repoLister)(nil)
	_ mcp.StatusProvider     = (*statusProvider)(nil)
	_ mcp.ConfigProvider     = (*configProvider)(nil)
	_ mcp.RepoRegistrar      = (*repoRegistrar)(nil)
)

// repoRegistrar adapts internal/repo's Add/Remove to the mcp.RepoRegistrar
// port consumed by eng_add_repo / eng_remove_repo. It lives in the composition
// root so internal/repo need not be imported by the MCP layer directly.
//
// AddRepo also fires a background cold scan against the freshly-registered
// repo so its working tree is indexed without a daemon restart (solov2-0z1.3)
// and seeds the fsnotify multi-repo watcher so subsequent live edits flow
// through Ingester.Save without a restart (solov2-id3). The scan runs under
// daemonCtx (not the caller's ctx) so a short-lived MCP request does not
// cancel the scan as soon as it returns. Outstanding scans are tracked on
// scanWG so the daemon's Stop can drain them under its budget.
type repoRegistrar struct {
	db        *sql.DB
	reparser  func(ctx context.Context, repo application.RepoRecord) error
	recordFor func(ctx context.Context, repoID string) (application.RepoRecord, error)
	// watchAdd seeds the live fsnotify watcher with a freshly-registered repo
	// so post-registration file edits are observed without a daemon restart.
	// Method-value of gitwatch.MultiRepoWatcher.Add in production wiring; nil
	// in legacy test wiring (the AddRepo path tolerates a nil watchAdd).
	watchAdd  func(repoID, rootPath string) error
	daemonCtx context.Context
	scanWG    *sync.WaitGroup
}

// AddRepo registers rootPath and returns the repo_id. repo.Add inserts the
// repos row and installs git hooks, then returns; on success a cold scan is
// dispatched in a background goroutine (bound to daemonCtx) so the caller is
// not blocked on potentially-long indexing work. A nil reparser or recordFor
// silently skips the dispatch (used in legacy wiring and in tests that do not
// exercise the cold-scan path).
func (rr *repoRegistrar) AddRepo(ctx context.Context, rootPath string) (string, bool, error) {
	repoID, existed, err := repo.Add(ctx, rr.db, rootPath)
	if err != nil {
		return "", false, err
	}

	// Seed the live fsnotify watcher before kicking off the cold scan so a
	// rapid edit between the cold scan finishing and the next loop iteration
	// is not lost. The watcher is idempotent (already-watched repos are a
	// no-op) so a redundant call from a future code path is safe. A failure
	// here is logged but not fatal — the cold scan still runs and live
	// watching can be recovered on the next daemon restart.
	if rr.watchAdd != nil {
		if werr := rr.watchAdd(repoID, rootPath); werr != nil {
			slog.Error("add-repo: live-watch seed",
				"repo_id", repoID, "root", rootPath, "err", werr)
		}
	}

	if rr.reparser == nil || rr.recordFor == nil {
		return repoID, existed, nil
	}

	// Skip the cold-scan dispatch for an already-registered repo: the existing
	// row already drove a scan on its original add (and continues to be kept
	// fresh by the post-commit hook / live watcher), so re-scanning here is
	// pure waste. (solov2-khjd)
	if existed {
		return repoID, existed, nil
	}

	// daemonCtx outlives any single request ctx, so the goroutine survives
	// the caller returning. Fall back to context.Background() if Start has
	// not yet wired one (defensive — production wiring always sets it).
	scanCtx := rr.daemonCtx
	if scanCtx == nil {
		scanCtx = context.Background()
	}

	if rr.scanWG != nil {
		rr.scanWG.Add(1)
	}
	go func() {
		if rr.scanWG != nil {
			defer rr.scanWG.Done()
		}
		rec, lerr := rr.recordFor(scanCtx, repoID)
		if lerr != nil {
			if !errors.Is(lerr, context.Canceled) {
				slog.Error("add-repo: lookup new record",
					"repo_id", repoID, "err", lerr)
			}
			return
		}
		if rec.RepoID == "" {
			slog.Error("add-repo: new record not found",
				"repo_id", repoID)
			return
		}
		if serr := rr.reparser(scanCtx, rec); serr != nil &&
			!errors.Is(serr, context.Canceled) {
			slog.Error("add-repo: cold scan",
				"repo_id", repoID, "err", serr)
		}
	}()
	return repoID, existed, nil
}

// RemoveRepo deregisters the repo identified by repoID. repo.Remove deletes
// the repos row (CASCADE drops nodes/edges) and removes installed hooks.
func (rr *repoRegistrar) RemoveRepo(ctx context.Context, repoID string) error {
	return repo.Remove(ctx, rr.db, repoID)
}

// repoLister adapts internal/repo's registry List to the
// application.RepoLister port consumed by the admin MCP tools. It lives in the
// composition root so internal/repo need not import internal/application.
type repoLister struct {
	db *sql.DB
}

// ListRepos returns every registered repository as an application.RepoRecord.
// repo.Record and application.RepoRecord are field-identical, so the mapping
// is a straight 1:1 copy.
func (rl *repoLister) ListRepos(ctx context.Context) ([]application.RepoRecord, error) {
	recs, err := repo.List(ctx, rl.db)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	out := make([]application.RepoRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, toAppRecord(r))
	}
	return out, nil
}

// toAppRecord projects a repo.Record (storage shape) to an
// application.RepoRecord (port shape). The two are field-identical today;
// the helper exists so a future field divergence is a single-site edit.
func toAppRecord(r repo.Record) application.RepoRecord {
	return application.RepoRecord{
		RepoID:          r.RepoID,
		RootPath:        r.RootPath,
		ActiveBranch:    r.ActiveBranch,
		LastPromotedSHA: r.LastPromotedSHA,
	}
}

// lookupAppRecord returns the freshly-inserted repo row as an
// application.RepoRecord. It is the recordFor callback wired into
// repoRegistrar so the cold-scan goroutine can build the RepoRecord it needs
// without re-implementing the repo.Record → application.RepoRecord mapping.
// An unknown repoID yields a zero record and nil error, matching repo.Get.
func lookupAppRecord(db *sql.DB) func(ctx context.Context, repoID string) (application.RepoRecord, error) {
	return func(ctx context.Context, repoID string) (application.RepoRecord, error) {
		rec, err := repo.Get(ctx, db, repoID)
		if err != nil {
			return application.RepoRecord{}, err
		}
		return toAppRecord(rec), nil
	}
}

// statusProvider implements mcp.StatusProvider by querying live daemon state
// from the SQLite read pool. The returned key set is a superset of the static
// fallback in tools_admin.go (status, schema_version, degraded_reasons), so
// callers that previously relied on the fallback keep working.
//
// scans is the optional in-flight cold-scan registry (solov2-pm5). When set,
// Status surfaces a 'scans_in_flight' key so programmatic consumers can see
// when a cold scan is running without tailing the log. Nil-safe — a zero
// scans field surfaces an empty list.
type statusProvider struct {
	db    *sql.DB
	scans *application.ScanTracker
}

// Status reports liveness, schema version, and queue depth. Any query error is
// returned rather than swallowed so a degraded daemon is not reported "ok".
func (sp *statusProvider) Status(ctx context.Context) (map[string]any, error) {
	// MAX(version) is NULL on an empty schema_migrations table; scan into a
	// NullInt64 and treat NULL as version 0.
	var ver sql.NullInt64
	if err := sp.db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM schema_migrations`,
	).Scan(&ver); err != nil {
		return nil, fmt.Errorf("query schema version: %w", err)
	}

	var repoCount int
	if err := sp.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM repos`,
	).Scan(&repoCount); err != nil {
		return nil, fmt.Errorf("query repo count: %w", err)
	}

	var pendingEmbeds int
	if err := sp.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_embedding_refs WHERE state = 'pending'`,
	).Scan(&pendingEmbeds); err != nil {
		return nil, fmt.Errorf("query pending embeds: %w", err)
	}

	// scans_in_flight: snapshot of cold scans the reparser is currently
	// running, populated via solov2-pm5's ScanTracker. Empty slice when
	// nothing is running OR when no tracker is wired (test / legacy
	// callers). Programmatic consumers can use this to display a "scan
	// in progress" spinner without tailing daemon.log for the
	// 'cold scan: starting' line.
	scansInFlight := sp.scans.Snapshot()

	// solov2-30sa: keep the rollup status aligned with the eng_search_semantic
	// 'embeddings_pending' signal. Returning {status: "ok", pending_embeds:
	// 4699} alongside search responses that already flag 'embeddings_pending'
	// is contradictory — the same backlog drove both, so both should reflect it.
	reasons := []string{}
	rollup := "ok"
	if pendingEmbeds > 0 {
		reasons = append(reasons, mcp.DegradedReasonEmbeddingsPending)
		rollup = "degraded"
	}

	return map[string]any{
		"status":           rollup,
		"schema_version":   int(ver.Int64), // NULL -> 0
		"repo_count":       repoCount,
		"pending_embeds":   pendingEmbeds,
		"scans_in_flight":  scansInFlight,
		"degraded_reasons": reasons,
	}, nil
}

// configProvider implements mcp.ConfigProvider, exposing the daemon's resolved
// runtime configuration.
type configProvider struct {
	cfg Config
}

// Config returns the effective daemon configuration. The Config struct holds
// no secret fields today (OllamaURL is a local, unauthenticated URL); should a
// credential field be added later, redact it here before returning.
func (cp *configProvider) Config(_ context.Context) (map[string]any, error) {
	embedder := elect.Marker(cp.cfg.VeskaHome)
	// solov2-ebvg: cfg.EmbedModel is only populated when the operator
	// explicitly sets VESKA_EMBED_MODEL (Ollama path). For the default
	// model2vec/static path it's "", which surfaces as an empty field
	// even though `embedder` carries the model id. Derive the model name
	// from the embedder marker when the explicit field is empty so the
	// two columns stay consistent.
	embedModel := cp.cfg.EmbedModel
	if embedModel == "" {
		embedModel = modelNameFromMarker(embedder)
	}
	return map[string]any{
		"veska_home":     cp.cfg.VeskaHome,
		"sqlite_path":    cp.cfg.SQLitePath,
		"cli_sock":       cp.cfg.CLISockPath,
		"mcp_sock":       cp.cfg.MCPSockPath,
		"vector_backend": string(cp.cfg.VectorBackend),
		"embedder":       embedder,
		"ollama_url":     cp.cfg.OllamaURL,
		"embed_model":    embedModel,
		// config_schema_version is the version of THIS config payload's
		// shape — distinct from eng_get_status's schema_version, which is
		// the SQLite migration version of the data store (solov2-d2x).
		"config_schema_version": 1,
		"degraded_reasons":      []string{},
	}, nil
}

// modelNameFromMarker extracts the model name from an embedder marker
// like "model2vec(potion-code-16M)" → "potion-code-16M". Returns the
// whole marker on no parens (e.g. "static-v2"), and "" for an empty
// input. Lives here, not on elect, because it's a presentation concern
// specific to eng_get_config's wire shape (solov2-ebvg).
func modelNameFromMarker(marker string) string {
	open := strings.IndexByte(marker, '(')
	close := strings.LastIndexByte(marker, ')')
	if open >= 0 && close > open {
		return marker[open+1 : close]
	}
	return marker
}
