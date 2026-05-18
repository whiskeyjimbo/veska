package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application"
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
type repoRegistrar struct {
	db *sql.DB
}

// AddRepo registers rootPath and returns the repo_id. repo.Add inserts the
// repos row and installs git hooks, then returns; the cold scan is driven
// asynchronously by the daemon's queue/watcher, so this call does not block on
// it.
func (rr *repoRegistrar) AddRepo(ctx context.Context, rootPath string) (string, error) {
	return repo.Add(ctx, rr.db, rootPath)
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
		out = append(out, application.RepoRecord{
			RepoID:          r.RepoID,
			RootPath:        r.RootPath,
			ActiveBranch:    r.ActiveBranch,
			LastPromotedSHA: r.LastPromotedSHA,
		})
	}
	return out, nil
}

// statusProvider implements mcp.StatusProvider by querying live daemon state
// from the SQLite read pool. The returned key set is a superset of the static
// fallback in tools_admin.go (status, schema_version, degraded_reasons), so
// callers that previously relied on the fallback keep working.
type statusProvider struct {
	db *sql.DB
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

	return map[string]any{
		"status":           "ok",
		"schema_version":   int(ver.Int64), // NULL -> 0
		"repo_count":       repoCount,
		"pending_embeds":   pendingEmbeds,
		"degraded_reasons": []string{},
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
	return map[string]any{
		"veska_home":       cp.cfg.VeskaHome,
		"sqlite_path":      cp.cfg.SQLitePath,
		"cli_sock":         cp.cfg.CLISockPath,
		"mcp_sock":         cp.cfg.MCPSockPath,
		"vector_backend":   string(cp.cfg.VectorBackend),
		"ollama_url":       cp.cfg.OllamaURL,
		"embed_model":      cp.cfg.EmbedModel,
		"schema_version":   1,
		"degraded_reasons": []string{},
	}, nil
}
