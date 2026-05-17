package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// ErrMissingDep is returned by newDaemon when a required collaborator cannot
// be constructed because a piece of configuration (path, env, etc.) is empty
// or invalid. The Name field identifies the missing dep so operators see a
// clear, actionable message instead of a nil-pointer panic.
type ErrMissingDep struct {
	Name string
	Why  string
}

func (e *ErrMissingDep) Error() string {
	if e.Why != "" {
		return fmt.Sprintf("daemon: missing dependency %q: %s", e.Name, e.Why)
	}
	return fmt.Sprintf("daemon: missing dependency %q", e.Name)
}

// Config carries the resolved runtime configuration for the daemon.
//
// All fields are optional in the sense that newDaemon will fall back to
// environment-backed defaults when zero. The composition root validates each
// resolved value and returns ErrMissingDep when a field that ultimately must
// be non-empty (e.g. SQLitePath) cannot be derived.
type Config struct {
	// VeskaHome is the data root (defaults to config.DefaultVectorDir()).
	VeskaHome string

	// SQLitePath is the location of veska.db. Defaults to <VeskaHome>/veska.db.
	SQLitePath string

	// CLISockPath / MCPSockPath are the Unix sockets for the JSON-RPC server.
	// Defaults to config.DaemonSockPath() and config.MCPSockPath().
	CLISockPath string
	MCPSockPath string

	// VectorBackend selects the VectorStorage implementation.
	// Defaults to env VESKA_VECTOR_BACKEND, then BackendSQLiteVec.
	VectorBackend vector.BackendKind

	// OllamaURL / EmbedModel select the embedding provider.
	// Defaults to env VESKA_OLLAMA_URL / VESKA_EMBED_MODEL, then localhost /
	// nomic-embed-text.
	OllamaURL  string
	EmbedModel string

	// MetricsEnabled / MetricsListen override the metrics listener settings
	// resolved from config.toml ([metrics]). They exist so a caller (notably
	// tests) can drive the Prometheus listener without writing a config file.
	// When MetricsEnabled is false the file config still applies; the override
	// only forces the listener on. MetricsListen, when non-empty, replaces the
	// configured listen address (use "127.0.0.1:0" to claim a free port).
	MetricsEnabled bool
	MetricsListen  string
}

// ResolveConfig fills in defaults (env, then hard-coded) on a partially
// populated Config. The returned Config never has empty string fields, so
// validation in newDaemon is just a non-nil check.
func ResolveConfig(c Config) Config {
	if c.VeskaHome == "" {
		c.VeskaHome = config.DefaultVectorDir()
	}
	if c.SQLitePath == "" {
		c.SQLitePath = filepath.Join(c.VeskaHome, "veska.db")
	}
	if c.CLISockPath == "" {
		c.CLISockPath = config.DaemonSockPath()
	}
	if c.MCPSockPath == "" {
		c.MCPSockPath = config.MCPSockPath()
	}
	if c.VectorBackend == "" {
		if env := os.Getenv("VESKA_VECTOR_BACKEND"); env != "" {
			c.VectorBackend = vector.BackendKind(env)
		} else {
			c.VectorBackend = vector.BackendSQLiteVec
		}
	}
	if c.OllamaURL == "" {
		if env := os.Getenv("VESKA_OLLAMA_URL"); env != "" {
			c.OllamaURL = env
		} else {
			c.OllamaURL = "http://localhost:11434"
		}
	}
	if c.EmbedModel == "" {
		if env := os.Getenv("VESKA_EMBED_MODEL"); env != "" {
			c.EmbedModel = env
		} else {
			c.EmbedModel = "nomic-embed-text"
		}
	}
	return c
}

// Daemon is the long-running process composition root: pools, MCP server,
// embedder worker, queue poller, fsnotify watcher. Start spawns goroutines;
// Stop cancels them and removes socket files. Both are idempotent.
type Daemon struct {
	cfg Config

	pools   *sqlite.Pools
	vectors ports.VectorStorage

	staging  *application.StagingArea
	gate     *application.IngestionGate
	ingester *application.Ingester
	promoter *application.Promoter

	embed   *embedder.Worker
	poller  *queue.Poller
	watcher *gitwatch.MultiRepoWatcher
	mcpsrv  *mcp.Server
	mcpReg  *mcp.Registry

	// metrics is the Prometheus metric set, non-nil only when the metrics
	// listener is enabled. It is threaded into every Metrics-aware consumer.
	metrics *observability.Metrics
	// metricsReg is the dedicated registry backing metrics; it is served by
	// the /metrics HTTP listener. Non-nil exactly when metrics is non-nil.
	metricsReg *prometheus.Registry
	// metricsListen is the resolved listen address; metricsCloser shuts the
	// HTTP listener down on Stop; metricsAddr is the actual bound address
	// (resolved after Start, so a ":0" listen yields the real port).
	metricsListen string
	metricsCloser io.Closer
	metricsAddr   string

	startOnce sync.Once
	stopOnce  sync.Once
	started   bool
	ctx       context.Context
	cancel    context.CancelFunc
	mcpDone   chan struct{}
	wDone     chan struct{}
}

// newDaemon builds the full collaborator graph from cfg. Every dep is
// validated; any failure produces a typed *ErrMissingDep without panicking.
// The returned Daemon is not yet running — call Start.
func newDaemon(cfg Config) (*Daemon, error) {
	cfg = ResolveConfig(cfg)

	// Load ~/.veska/config.toml (defaults < config.toml < env vars). A missing
	// file is not an error. Selected values below are read from this surface
	// instead of compile-time constants; see docs/operations/CONFIG-SURFACE.md.
	fileCfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("daemon: load config: %w", err)
	}

	// Prometheus metrics. The metric set is constructed (and registered on the
	// default registry) only when metrics are enabled — either via config.toml
	// ([metrics] enabled) or the Config.MetricsEnabled override. A nil *Metrics
	// is threaded into the Metrics-aware consumers when disabled; they are all
	// nil-safe. The HTTP listener itself is bound later, in Start.
	metricsEnabled := fileCfg.Metrics.Enabled || cfg.MetricsEnabled
	metricsListen := fileCfg.Metrics.Listen
	if cfg.MetricsListen != "" {
		metricsListen = cfg.MetricsListen
	}
	var (
		metrics    *observability.Metrics
		metricsReg *prometheus.Registry
	)
	if metricsEnabled {
		metricsReg = prometheus.NewRegistry()
		metrics = observability.NewMetrics(metricsReg)
	}

	// Gate the review-pipeline LLM provider: only local Ollama is supported in
	// V2.0; hosted providers are deferred to V2.0.1. A misconfigured provider
	// fails fast here rather than surfacing as a confusing downstream error.
	if err := checkLLMProvider(fileCfg); err != nil {
		return nil, err
	}

	// Validate backend kind early so bad env doesn't surface as a confusing
	// downstream open error.
	switch cfg.VectorBackend {
	case vector.BackendSQLiteVec, vector.BackendUsearch:
	default:
		return nil, &ErrMissingDep{Name: "vector_backend",
			Why: fmt.Sprintf("unknown VESKA_VECTOR_BACKEND %q (want %q or %q)",
				cfg.VectorBackend, vector.BackendSQLiteVec, vector.BackendUsearch)}
	}

	if cfg.SQLitePath == "" {
		return nil, &ErrMissingDep{Name: "sqlite_path"}
	}
	if cfg.CLISockPath == "" {
		return nil, &ErrMissingDep{Name: "cli_sock_path"}
	}
	if cfg.MCPSockPath == "" {
		return nil, &ErrMissingDep{Name: "mcp_sock_path"}
	}
	if cfg.EmbedModel == "" {
		return nil, &ErrMissingDep{Name: "embed_model"}
	}

	// Make sure the SQLite parent directory exists; sqlite.Open does not
	// mkdir for us.
	if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o755); err != nil {
		return nil, fmt.Errorf("daemon: mkdir sqlite dir: %w", err)
	}

	// Open pools (read + write-hot + write-embed).
	pools, err := sqlite.OpenPools(cfg.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("daemon: open sqlite pools: %w", err)
	}
	// Apply migrations on the hot-write pool.
	if _, err := sqlite.OpenWithOptions(cfg.SQLitePath, sqlite.Options{}); err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: migrate sqlite: %w", err)
	}

	// Vector backend.
	vec, err := vector.NewVectorStorage(cfg.VectorBackend, cfg.VeskaHome)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: open vector storage: %w", err)
	}

	// Application core (parse + stage + ingestion gate).
	staging := application.NewStagingArea()
	gate := application.NewIngestionGate(staging)

	parser := treesitter.NewGoParser()
	ingester := application.NewIngester(parser, staging, gate)
	findings := sqlite.NewFindingRepo(pools.WriteHot)
	ingester.SetFindingStorage(findings)

	// Promoter + structural check pipeline. The PromotionStore owns the atomic
	// promotion transaction; co-transactional sinks (FTS, embedding-refs) are
	// registered here at start-up — a future sink is one more arg.
	promotionStore := sqlite.NewPromotionStore(
		pools.WriteHot,
		[]sqlite.PromotionSink{
			sqlite.NewFTSSink(),
			sqlite.NewEmbedRefSink(),
		},
		sqlite.WithReviewEnabled(fileCfg.Review.Enabled),
	)
	promoter := application.NewPromoter(staging, promotionStore)
	checkReg := checks.NewRegistry()
	deadcodeRepo := sqlite.NewDeadCodeRepo(pools.ReadDB)
	contractRepo := sqlite.NewContractDriftRepo(pools.ReadDB)
	checkReg.Register(checks.NewDeadCodeCheck(deadcodeRepo))
	checkReg.Register(checks.NewContractDriftCheck(contractRepo))
	runner := checks.NewRunner(checkReg, findings, metrics)
	promoter.SetCheckRunner(checkRunnerAdapter{inner: runner})

	// Embedding provider + embedder worker.
	provider, err := ollama.New(cfg.EmbedModel, ollama.WithBaseURL(cfg.OllamaURL))
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: embedding provider: %w", err)
	}
	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.WriteEmbed)
	embedWorker, err := embedder.NewWorker(refs, provider, vec,
		embedder.WithRatePerSec(fileCfg.Embedder.RatePerSec),
		embedder.WithMaxAttempts(embedder.DefaultMaxAttempts),
		embedder.WithMetrics(metrics),
	)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: embedder worker: %w", err)
	}

	// Queue handlers (autolink + revalidate). The embedder is driven by its
	// own poll loop today (embedder.Worker), but we still register an embed
	// handler so the Poller's WorkKindEmbed lane drains without leaving rows
	// pending if other code paths enqueue them.
	nodeLookup := sqlite.NewNodeLookupRepo(pools.ReadDB)
	edgeRepo := sqlite.NewEdgeRepo(pools.WriteHot)
	linker, err := autolink.NewLinker(refs, vec, autolink.WithMetrics(metrics))
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: autolink linker: %w", err)
	}
	autoH, err := autolink.NewHandler(linker, nodeLookup, edgeRepo, findings)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: autolink handler: %w", err)
	}
	revalRepo := sqlite.NewRevalidateRepo(pools.WriteHot)
	revalH := revalidate.NewHandler(revalRepo, revalidate.WithMetrics(metrics))

	// Wiki regeneration handler. The WorkKindWiki lane regenerates both the
	// hot_zone and entry_points Markdown pages after every promotion and
	// stamps the last-render time into daemon_state.
	wikiEdges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	wikiGraph := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	wikiFindings := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	wikiBlast := blastradius.NewService(wikiEdges, nodeLookup, staging)
	wikiCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	hotZoneSvc, err := wiki.NewHotZoneService(wikiCounts, nodeLookup.NodesInFile, wikiBlast)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: wiki hot-zone service: %w", err)
	}
	epSvc, err := wiki.NewEntryPointsService(
		wikiGraph.LoadGraph, wikiEdges.InboundEdges, wikiFindings.OpenFindingNodeIDs, wikiBlast,
	)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: wiki entry-points service: %w", err)
	}
	wikiRoot := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(pools.ReadDB)(ctx, repoID)
	}
	wikiH, err := wiki.NewHandler(
		hotZoneSvc, epSvc,
		sqlite.NewWikiRenderStateRepo(pools.ReadDB, pools.WriteHot),
		wikiRoot,
	)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: wiki handler: %w", err)
	}

	handlers := map[queue.WorkKind]queue.WorkHandler{
		ports.WorkKindAutoLink:   autoH,
		ports.WorkKindRevalidate: revalH,
		ports.WorkKindWiki:       wikiH,
		ports.WorkKindEmbed:      noopEmbedHandler{}, // drained by embed worker
	}

	// Optional review lane. The WorkKindReview handler is registered only when
	// review is enabled; the promotion store enqueues review rows under the
	// same gate, so a disabled review pipeline has neither producer nor lane.
	if fileCfg.Review.Enabled {
		reviewLoader, lerr := review.NewLoader()
		if lerr != nil {
			_ = pools.Close()
			return nil, fmt.Errorf("daemon: review prompt loader: %w", lerr)
		}
		var genOpts []llm.Option
		if d, derr := time.ParseDuration(fileCfg.LLMGenerator.Timeout); derr == nil && d > 0 {
			genOpts = append(genOpts, llm.WithTimeout(d))
		}
		reviewGen := llm.NewOllamaGenerator(
			fileCfg.LLMGenerator.Endpoint, fileCfg.LLMGenerator.Model, nil, genOpts...)
		reviewRoot := func(ctx context.Context, repoID string) (string, error) {
			return repoRootFunc(pools.ReadDB)(ctx, repoID)
		}

		// Token-quota enforcement (solov2-nz2.5): the per-day total persists
		// in daemon_state; the audit writer records the one-line entry when
		// the daily-cap pause trips.
		tokenStore := sqlite.NewReviewTokenStore(pools.ReadDB, pools.WriteHot)
		quota := review.NewQuota(
			fileCfg.Review.MaxTokensPerCommit,
			fileCfg.Review.MaxTokensPerDay,
			tokenStore, nil)
		auditW, aerr := audit.NewAuditFileWriter(
			filepath.Join(config.DefaultVectorDir(), "audit.jsonl"))
		if aerr != nil {
			_ = pools.Close()
			return nil, fmt.Errorf("daemon: review audit writer: %w", aerr)
		}

		reviewOpts := []review.HandlerOption{
			review.WithQuota(quota), review.WithAuditWriter(auditW),
		}
		if metrics != nil {
			reviewOpts = append(reviewOpts,
				review.WithErrorCounter(metricsErrorCounter{m: metrics}))
		}
		reviewH, rerr := review.NewHandler(reviewGen, reviewLoader, reviewRoot, findings,
			reviewOpts...)
		if rerr != nil {
			_ = pools.Close()
			return nil, fmt.Errorf("daemon: review handler: %w", rerr)
		}
		handlers[ports.WorkKindReview] = reviewH
	}
	// Post-promotion queue poll cadence comes from config.toml; an
	// unparseable value falls back to the queue package default.
	pollInterval := 250 * time.Millisecond
	if d, derr := time.ParseDuration(fileCfg.PostPromotionQueue.PollInterval); derr == nil && d > 0 {
		pollInterval = d
	}
	poller := queue.NewWithInterval(pools.ReadDB, pools.WriteHot, handlers, pollInterval)

	// fsnotify multi-repo watcher.
	watcher := gitwatch.NewMultiRepoWatcher()

	// MCP server. The Registry implements mcp.Handler.
	registry := mcp.NewRegistry()
	registerMCPTools(registry, mcpDeps{
		pools:    pools,
		cfg:      cfg,
		staging:  staging,
		vectors:  vec,
		provider: provider,
		refs:     refs,
		metrics:  metrics,
	})
	mcpsrv := mcp.NewServer(cfg.CLISockPath, cfg.MCPSockPath, registry)

	return &Daemon{
		cfg:           cfg,
		pools:         pools,
		vectors:       vec,
		staging:       staging,
		gate:          gate,
		ingester:      ingester,
		promoter:      promoter,
		embed:         embedWorker,
		poller:        poller,
		watcher:       watcher,
		mcpsrv:        mcpsrv,
		mcpReg:        registry,
		metrics:       metrics,
		metricsReg:    metricsReg,
		metricsListen: metricsListen,
	}, nil
}

// metricsErrorCounter adapts *observability.Metrics to the review.ErrorCounter
// port: IncError bumps the veska_error_count series for the given kind label.
// It is only constructed when metrics are enabled, so m is always non-nil.
type metricsErrorCounter struct {
	m *observability.Metrics
}

func (c metricsErrorCounter) IncError(kind string) {
	c.m.ErrorCount.WithLabelValues(kind).Inc()
}

// mcpRegistry exposes the daemon's MCP tool registry. It is used by tests to
// assert which tool families are wired; production code reaches the registry
// through the MCP server.
func (d *Daemon) mcpRegistry() *mcp.Registry { return d.mcpReg }

// checkRunnerAdapter bridges *checks.Runner (which uses checks.Input) to the
// application.CheckRunner interface (which uses application.CheckRunInput).
// The two structs are field-identical; the indirection exists so the
// application package does not need to import the checks sub-package.
type checkRunnerAdapter struct {
	inner *checks.Runner
}

func (a checkRunnerAdapter) Run(ctx context.Context, in application.CheckRunInput) {
	a.inner.Run(ctx, checks.Input{
		RepoID:    in.RepoID,
		Branch:    in.Branch,
		GitSHA:    in.GitSHA,
		FilePaths: in.FilePaths,
	})
}

// noopEmbedHandler keeps the embed queue lane drained when the embedder
// worker reads its refs out-of-band. It marks rows as handled without doing
// any work. Once the embedder is migrated to the Poller this can be replaced
// with the real handler.
type noopEmbedHandler struct{}

func (noopEmbedHandler) Handle(_ context.Context, _ ports.WorkRow) error { return nil }

// mcpDeps bundles the collaborators registerMCPTools needs beyond the SQLite
// pools and Config. They are all constructed by newDaemon; passing them as a
// struct keeps the call site readable as the tool surface grows.
type mcpDeps struct {
	pools    *sqlite.Pools
	cfg      Config
	staging  *application.StagingArea
	vectors  ports.VectorStorage
	provider ports.EmbeddingProvider
	refs     *sqlite.EmbeddingRefsRepo
	metrics  *observability.Metrics
}

// registerMCPTools wires every tool family into the registry: findings,
// suppressions, tasks, owners, todos, admin, plus the graph / blast-radius /
// semantic-search families backed by the SQLite GraphRepo and the application
// blastradius + search services.
func registerMCPTools(r *mcp.Registry, d mcpDeps) {
	pools := d.pools

	// Tools that only need *sql.DB + AuditWriter.
	mcp.RegisterFindingTools(r, pools.WriteHot, nil)
	mcp.RegisterSuppressionTools(r, pools.WriteHot, nil)
	mcp.RegisterRecordTools(r, pools.WriteHot, nil)
	mcp.RegisterRepoTools(r, &repoRegistrar{db: pools.WriteHot})
	mcp.RegisterTaskTools(r, pools.WriteHot, nil)
	mcp.RegisterOwnerTools(r, pools.WriteHot)
	mcp.RegisterTodoTools(r, sqlite.NewTodoQuerierRepo(pools.ReadDB))
	// Admin tools: repo listing + live status/config from the read pool and
	// the resolved daemon Config.
	mcp.RegisterAdminTools(r,
		&repoLister{db: pools.ReadDB},
		&statusProvider{db: pools.ReadDB},
		&configProvider{cfg: d.cfg},
	)

	// Graph tools backed by the SQLite GraphRepo adapter. Writes take the
	// hot-write pool; reads take the read pool.
	graph := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	mcp.RegisterGraphTools(r, graph, d.staging)

	// Blast-radius tools. The Service walks edge adjacency + staging; the
	// repoRoot lookup resolves a repoID to its working tree, and changedFiles
	// is the git HEAD diff.
	edges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	blastSvc := blastradius.NewService(edges, nodes, d.staging)
	mcp.RegisterBlastTools(r, blastSvc, repoRootFunc(pools.ReadDB), gitwatch.ChangedFiles)

	// Wiki hot_zone surface. Change frequency comes from the git commit-history
	// reader over the default look-back window; blast radius reuses blastSvc.
	hotZoneCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	if hotZoneSvc, err := wiki.NewHotZoneService(hotZoneCounts, nodes.NodesInFile, blastSvc); err == nil {
		mcp.RegisterWikiTools(r, hotZoneSvc, repoRootFunc(pools.ReadDB))
	} else {
		mcp.RegisterWikiTools(r, nil, nil)
	}

	// Wiki entry_points surface. Candidates are enumerated from the loaded
	// graph; the three safety gates draw on edge adjacency, blast radius and
	// the findings table.
	graphForEP := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	findingQuerier := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	if epSvc, err := wiki.NewEntryPointsService(
		graphForEP.LoadGraph, edges.InboundEdges, findingQuerier.OpenFindingNodeIDs, blastSvc,
	); err == nil {
		mcp.RegisterEntryPointsTool(r, epSvc)
	} else {
		mcp.RegisterEntryPointsTool(r, nil)
	}

	// Context-pack surface. Assembles a token-bounded bundle of relevant
	// nodes / commits / findings / tasks for a symbol or a task; commits
	// come from the git commit-history reader, the active task from the
	// tasks table.
	fileHistory := func(ctx context.Context, repoRoot, path string, window time.Duration) ([]contextpack.CommitInfo, error) {
		commits, err := gitwatch.FileHistory(ctx, repoRoot, path, window)
		if err != nil {
			return nil, err
		}
		out := make([]contextpack.CommitInfo, 0, len(commits))
		for _, c := range commits {
			out = append(out, contextpack.CommitInfo{
				Hash: c.Hash, Author: c.Author, When: c.When, Subject: c.Subject,
			})
		}
		return out, nil
	}
	if cpAsm, err := contextpack.NewAssembler(
		graph.FindNodes, blastSvc, fileHistory, findingQuerier.OpenFindingNodeIDs,
		gitwatch.ChangedFiles, nodes.NodesInFile, activeTaskFunc(pools.ReadDB),
	); err == nil {
		mcp.RegisterContextPackTool(r, cpAsm, repoRootFunc(pools.ReadDB))
	} else {
		mcp.RegisterContextPackTool(r, nil, nil)
	}

	// Semantic-search tools. The Service orchestrates embed → vector search →
	// node hydration with lexical fallback.
	searchSvc := search.NewService(d.provider, d.vectors, nodes,
		search.WithMetrics(d.metrics))
	mcp.RegisterSearchTools(r, searchSvc, d.refs, d.vectors, nodes)
}

// activeTaskFunc returns a contextpack.ActiveTaskFunc reading the repo's
// active task from the tasks table — the same table tools_tasks.go owns.
// No active task yields (nil, nil) rather than an error.
func activeTaskFunc(db *sql.DB) contextpack.ActiveTaskFunc {
	return func(ctx context.Context, repoID string) (*contextpack.TaskInfo, error) {
		var (
			t                   contextpack.TaskInfo
			tracker, trackerRef sql.NullString
			active              int
		)
		err := db.QueryRowContext(ctx,
			`SELECT task_id, repo_id, tracker, tracker_ref, title, active
			   FROM tasks WHERE repo_id = ? AND active = 1`,
			repoID,
		).Scan(&t.TaskID, &t.RepoID, &tracker, &trackerRef, &t.Title, &active)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("active task lookup: %w", err)
		}
		t.Tracker = tracker.String
		t.TrackerRef = trackerRef.String
		t.Active = active != 0
		return &t, nil
	}
}

// repoRootFunc returns an mcp.RepoRootFunc that resolves a repoID to its
// registered working-tree path. An unknown repoID yields an error so the
// blast-radius handler surfaces a clear "repo not registered" message rather
// than running against an empty path.
func repoRootFunc(db *sql.DB) mcp.RepoRootFunc {
	return func(ctx context.Context, repoID string) (string, error) {
		records, err := repo.List(ctx, db)
		if err != nil {
			return "", fmt.Errorf("repo root lookup: %w", err)
		}
		for _, rec := range records {
			if rec.RepoID == repoID {
				return rec.RootPath, nil
			}
		}
		return "", fmt.Errorf("repo root lookup: repo %q is not registered", repoID)
	}
}

// Start launches all background goroutines. It is safe to call multiple times
// — subsequent invocations are no-ops.
func (d *Daemon) Start(ctx context.Context) error {
	var startErr error
	d.startOnce.Do(func() {
		d.started = true
		d.ctx, d.cancel = context.WithCancel(ctx)
		d.mcpDone = make(chan struct{})
		d.wDone = make(chan struct{})

		// Prometheus metrics HTTP listener. Bound only when metrics are
		// enabled; the closer is shut down in Stop. A bind failure is logged,
		// not fatal — a daemon without a metrics endpoint is still a valid
		// running daemon.
		if d.metrics != nil {
			closer, addr, err := observability.StartHTTPListener(d.metricsListen, d.metricsReg)
			if err != nil {
				slog.Error("daemon: metrics listener", "addr", d.metricsListen, "err", err)
			} else {
				d.metricsCloser = closer
				d.metricsAddr = addr
				slog.Info("daemon: metrics listener bound", "addr", addr)
			}
		}

		// MCP server (its own goroutine; Start blocks until ctx is done).
		go func() {
			defer close(d.mcpDone)
			if err := d.mcpsrv.Start(d.ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("daemon: mcp server exited", "err", err)
			}
		}()

		// Wait briefly for the listener sockets to appear so callers can rely
		// on them being present after Start returns. The MCP server creates
		// them synchronously inside Start, so 250ms is plenty.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			_, errCLI := os.Stat(d.cfg.CLISockPath)
			_, errMCP := os.Stat(d.cfg.MCPSockPath)
			if errCLI == nil && errMCP == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Embedder worker.
		d.embed.Start(d.ctx)

		// Queue poller.
		d.poller.Start(d.ctx)

		// fsnotify multiplexer.
		d.watcher.Start(d.ctx)

		// Seed the watcher with every registered repository. A failed or
		// empty listing is logged, not fatal — a daemon watching zero repos
		// is still a valid running daemon.
		if repos, err := repo.List(d.ctx, d.pools.ReadDB); err != nil {
			slog.Error("daemon: list repos for watcher", "err", err)
		} else {
			for _, r := range repos {
				if err := d.watcher.Add(r.RepoID, r.RootPath); err != nil {
					slog.Error("daemon: watch repo", "repo", r.RepoID, "err", err)
				}
			}
		}

		// Bridge filesystem events → Ingester.Save.
		go func() {
			defer close(d.wDone)
			d.runWatchLoop(d.ctx)
		}()
	})
	return startErr
}

// runWatchLoop reads from the multi-repo watcher and forwards each file event
// to Ingester.Save. The loop terminates when ctx is cancelled or Events()
// closes.
func (d *Daemon) runWatchLoop(ctx context.Context) {
	events := d.watcher.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := os.ReadFile(ev.Event.Path)
			if err != nil {
				continue
			}
			d.ingester.Save(ctx, ev.RepoID, "main", ev.Event.Path, data)
		}
	}
}

// Stop cancels the daemon's context, waits for background goroutines to
// exit, closes the SQLite pools, and removes the Unix sockets. Idempotent.
func (d *Daemon) Stop() error {
	var stopErr error
	d.stopOnce.Do(func() {
		if d.cancel != nil {
			d.cancel()
		}
		// Wait with a bounded budget so a stuck goroutine cannot wedge
		// shutdown forever; we still close the pool so the next start is
		// not blocked on a stale lock.
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()

		if d.mcpDone != nil {
			select {
			case <-d.mcpDone:
			case <-timeout.C:
			}
		}
		if d.wDone != nil {
			select {
			case <-d.wDone:
			case <-time.After(500 * time.Millisecond):
			}
		}

		// Shut the metrics HTTP listener down gracefully so its serve
		// goroutine exits and the port is released for the next start.
		if d.metricsCloser != nil {
			if err := d.metricsCloser.Close(); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("daemon: close metrics listener: %w", err))
			}
		}

		if d.started && d.embed != nil {
			d.embed.Stop()
		}
		if d.started && d.poller != nil {
			d.poller.Wait()
		}

		if d.pools != nil {
			if err := d.pools.Close(); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("daemon: close pools: %w", err))
			}
		}

		// Best-effort socket cleanup (MCP server already removes them, but
		// belt-and-braces if it crashed before reaching its defer).
		_ = os.Remove(d.cfg.CLISockPath)
		_ = os.Remove(d.cfg.MCPSockPath)
	})
	return stopErr
}
