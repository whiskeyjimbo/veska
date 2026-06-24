// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/application/fts"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/application/summary"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// buildQueueHandlers builds the post-promotion work handlers (autolink,
// revalidate, wiki, the no-op embed drain, and the optional review lane) into
// the handlers map consumed by the poller.
func (b *daemonBuilder) buildQueueHandlers() error {
	autoH, err := b.buildAutolinkHandler()
	if err != nil {
		return err
	}
	revalH, err := revalidate.NewHandler(sqlite.NewRevalidateRepo(b.pools.Write), revalidate.WithMetrics(b.metrics))
	if err != nil {
		return fmt.Errorf("revalidate handler: %w", err)
	}
	wikiH, err := b.buildWikiHandler()
	if err != nil {
		return err
	}
	ftsH, err := fts.NewHandler(sqlite.NewFTSReindexRepo(b.pools.Write))
	if err != nil {
		return fmt.Errorf("fts handler: %w", err)
	}
	b.handlers = map[queue.WorkKind]queue.WorkHandler{
		ports.WorkKindAutoLink:   autoH,
		ports.WorkKindRevalidate: revalH,
		ports.WorkKindWiki:       wikiH,
		ports.WorkKindFTS:        ftsH,
		ports.WorkKindEmbed:      noopEmbedHandler{}, // drained by embed worker
	}
	if b.fileCfg.Review.Enabled {
		reviewH, rerr := b.buildReviewHandler()
		if rerr != nil {
			return rerr
		}
		b.handlers[ports.WorkKindReview] = reviewH
	}
	if b.fileCfg.Summary.Enabled {
		summaryH, serr := b.buildSummaryHandler()
		if serr != nil {
			return serr
		}
		b.handlers[ports.WorkKindSummary] = summaryH
	}
	return nil
}

// buildAutolinkHandler wires the SIMILAR_TO autolink handler; the repo-kind
// lookup skips ephemeral (cache-tier) repos.
func (b *daemonBuilder) buildAutolinkHandler() (*autolink.Handler, error) {
	nodeLookup := sqlite.NewNodeLookupRepo(b.pools.ReadDB)
	edgeRepo := sqlite.NewEdgeRepo(b.pools.Write)
	linker, err := autolink.NewLinker(b.refs, b.vec,
		autolink.WithMetrics(b.metrics),
		autolink.WithThreshold(float32(b.fileCfg.Autolink.Threshold)),
		autolink.WithTopK(b.fileCfg.Autolink.TopK))
	if err != nil {
		return nil, fmt.Errorf("daemon: autolink linker: %w", err)
	}
	autoH, err := autolink.NewHandler(linker, nodeLookup, edgeRepo, b.findings,
		autolink.WithRepoKindLookup(func(ctx context.Context, repoID string) (string, error) {
			rec, gerr := repo.Get(ctx, b.pools.ReadDB, repoID)
			if gerr != nil {
				return "", gerr
			}
			return rec.Kind, nil
		}),
		autolink.WithEmbedReadiness(b.refs),
	)
	if err != nil {
		return nil, fmt.Errorf("daemon: autolink handler: %w", err)
	}
	return autoH, nil
}

// buildWikiHandler wires the WorkKindWiki regeneration handler (hot_zone +
// entry_points pages) via the shared composition constructor. It shares the
// live staging so blast radius sees in-flight nodes, and honors [wiki].write_pages.
func (b *daemonBuilder) buildWikiHandler() (*wiki.Handler, error) {
	return composition.NewWikiHandler(b.pools, b.staging, repoRootFunc(b.pools.ReadDB), composition.WithWritePages(b.fileCfg.Wiki.WritePages))
}

// buildReviewHandler wires the optional WorkKindReview lane (Ollama generator,
// prompt loader, per-commit/per-day token quota, audit writer); review-enabled only.
func (b *daemonBuilder) buildReviewHandler() (queue.WorkHandler, error) {
	reviewLoader, err := review.NewLoader()
	if err != nil {
		return nil, fmt.Errorf("daemon: review prompt loader: %w", err)
	}
	var genOpts []llm.Option
	if d, derr := time.ParseDuration(b.fileCfg.LLMGenerator.Timeout); derr == nil && d > 0 {
		genOpts = append(genOpts, llm.WithTimeout(d))
	}
	reviewGen := llm.NewOllamaGenerator(
		b.fileCfg.LLMGenerator.Model,
		append([]llm.Option{llm.WithBaseURL(b.fileCfg.LLMGenerator.Endpoint)}, genOpts...)...)
	reviewRoot := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(b.pools.ReadDB)(ctx, repoID)
	}
	// Token-quota: the per-day total persists in daemon_state; the audit writer
	// records the daily-cap pause.
	tokenStore := sqlite.NewReviewTokenStore(b.pools.ReadDB, b.pools.Write)
	quota := review.NewQuota(
		b.fileCfg.Review.MaxTokensPerCommit,
		b.fileCfg.Review.MaxTokensPerDay,
		tokenStore)
	auditW, err := audit.NewAuditFileWriter(
		filepath.Join(config.DefaultVectorDir(), "audit.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("daemon: review audit writer: %w", err)
	}
	reviewOpts := []review.HandlerOption{
		review.WithQuota(quota), review.WithAuditWriter(auditW),
	}
	if b.metrics != nil {
		reviewOpts = append(reviewOpts,
			review.WithErrorCounter(metricsErrorCounter{m: b.metrics}))
	}
	reviewH, err := review.NewHandler(reviewGen, reviewLoader, reviewRoot, b.findings, reviewOpts...)
	if err != nil {
		return nil, fmt.Errorf("daemon: review handler: %w", err)
	}
	return reviewH, nil
}

// buildSummaryHandler wires the optional WorkKindSummary lane: the Ollama
// generator (shared [llm_generator] slot) and the node short_summary store.
// Only called when summary is enabled.
func (b *daemonBuilder) buildSummaryHandler() (queue.WorkHandler, error) {
	var genOpts []llm.Option
	if d, derr := time.ParseDuration(b.fileCfg.LLMGenerator.Timeout); derr == nil && d > 0 {
		genOpts = append(genOpts, llm.WithTimeout(d))
	}
	gen := llm.NewOllamaGenerator(
		b.fileCfg.LLMGenerator.Model,
		append([]llm.Option{llm.WithBaseURL(b.fileCfg.LLMGenerator.Endpoint)}, genOpts...)...)
	root := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(b.pools.ReadDB)(ctx, repoID)
	}
	store := sqlite.NewSummaryStore(b.pools.ReadDB, b.pools.Write)

	opts := []summary.HandlerOption{summary.WithGeneratorName(b.fileCfg.LLMGenerator.Model)}
	auditW, err := audit.NewAuditFileWriter(
		filepath.Join(config.DefaultVectorDir(), "audit.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("daemon: summary audit writer: %w", err)
	}
	opts = append(opts, summary.WithAuditWriter(auditW))

	summaryH, err := summary.NewHandler(gen, store, root, opts...)
	if err != nil {
		return nil, fmt.Errorf("daemon: summary handler: %w", err)
	}
	return summaryH, nil
}
