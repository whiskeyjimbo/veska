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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/changedsymbols"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/application/vulnrefresh"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/resolver"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"github.com/whiskeyjimbo/veska/internal/repo"
	"github.com/whiskeyjimbo/veska/internal/savings"
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
	// Defaults to config.CLISockPath() and config.MCPSockPath().
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

	// TracingEnabled / TracingEndpoint override the OTLP tracing settings
	// resolved from config.toml ([tracing]). They exist so a caller (notably
	// tests) can drive the tracer without writing a config file. When
	// TracingEnabled is false the file config still applies; the override only
	// forces tracing on. TracingEndpoint, when non-empty, replaces the
	// configured OTLP endpoint.
	TracingEnabled  bool
	TracingEndpoint string
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
		c.CLISockPath = config.CLISockPath()
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
	// EmbedModel is only consulted when the elected embedder is Ollama; it
	// is no longer defaulted to nomic-embed-text here, since that implied
	// Ollama-embedding was the daemon default (it isn't — see elect). When
	// VESKA_EMBEDDER=ollama and this is unset, elect supplies the default.
	if c.EmbedModel == "" {
		c.EmbedModel = os.Getenv("VESKA_EMBED_MODEL")
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

	// resync is the startup-resync orchestrator: on Start it walks every
	// registered repo and either replays missed commits or full-reparses
	// (never-promoted / divergent SHA). Its Run is launched in its own
	// goroutine so it never blocks Start; Stop waits on resyncDone.
	resync *application.StartupResync

	// vulnRefresher keeps the OSV advisory cache fresh. It is non-nil only
	// when [vuln_source] provider="osv"; Start launches its blocking Run on
	// the daemon's lifetime context.
	vulnRefresher *vulnrefresh.Refresher

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

	// tracerProvider is the OTLP TracerProvider, non-nil only when tracing is
	// enabled and an endpoint is configured. It is threaded into every
	// tracing-aware consumer and shut down (flush + exporter close) in Stop.
	tracerProvider *sdktrace.TracerProvider

	// savingsRec records per-search token-savings telemetry to
	// <VeskaHome>/savings.jsonl (solov2-3bu). Nil disables recording.
	// Closed on Stop so the underlying file handle is released.
	savingsRec *savings.Recorder

	startOnce sync.Once
	stopOnce  sync.Once
	started   bool
	ctx       context.Context
	cancel    context.CancelFunc
	mcpDone   chan struct{}
	wDone     chan struct{}
	// resyncDone is closed when the startup-resync goroutine returns.
	// Stop waits on it with the same bounded budget as wDone so a slow
	// resync cannot wedge shutdown.
	resyncDone chan struct{}

	// regSvc is the live repoRegistrar wired with the cold-scan reparser
	// and the post-Start daemonCtx. It is built in newDaemon (the closure
	// graph is available there) and re-bound to d.ctx in Start so the
	// dispatched cold-scan goroutine outlives any single MCP request ctx.
	regSvc *repoRegistrar

	// scanWG tracks in-flight AddRepo cold-scan goroutines so Stop can
	// drain them under the same bounded budget as the other background
	// workers. Pointer so it can be shared with regSvc (built before the
	// Daemon struct itself in newDaemon).
	scanWG *sync.WaitGroup
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

	// OTLP tracing. The TracerProvider is constructed only when tracing is
	// enabled — either via config.toml ([tracing] enabled) or the
	// Config.TracingEnabled override — AND an OTLP endpoint is set. The
	// both-or-neither rule is a fatal startup error: enabling tracing with no
	// endpoint, or setting an endpoint with tracing disabled, both fail here
	// so the operator's intent is never silently ignored. config.Validate
	// covers the file surface; this re-check also covers the test overrides.
	tracingEnabled := fileCfg.Tracing.Enabled || cfg.TracingEnabled
	tracingEndpoint := fileCfg.Tracing.OTLPEndpoint
	if cfg.TracingEndpoint != "" {
		tracingEndpoint = cfg.TracingEndpoint
	}
	if tracingEnabled && tracingEndpoint == "" {
		return nil, &ErrMissingDep{Name: "tracing.otlp_endpoint",
			Why: "tracing is enabled but no OTLP endpoint is set (set tracing.otlp_endpoint or VESKA_OTLP_ENDPOINT)"}
	}
	if !tracingEnabled && tracingEndpoint != "" {
		return nil, &ErrMissingDep{Name: "tracing.enabled",
			Why: "an OTLP endpoint is set but tracing is disabled (set tracing.enabled = true or clear the endpoint)"}
	}
	var tracerProvider *sdktrace.TracerProvider
	if tracingEnabled {
		tp, terr := observability.NewTracerProvider(tracingEndpoint)
		if terr != nil {
			return nil, fmt.Errorf("daemon: construct tracer provider: %w", terr)
		}
		tracerProvider = tp
	}

	// Gate the review-pipeline LLM provider: only local Ollama is supported in
	// V2.0; hosted providers are deferred to V2.0.1. A misconfigured provider
	// fails fast here rather than surfacing as a confusing downstream error.
	if err := checkLLMProvider(fileCfg); err != nil {
		return nil, err
	}

	// Gate the vulnerability advisory source provider. Only OSV is supported;
	// an unknown provider fails fast here. An absent [vuln_source] section
	// leaves the feature off (NullVulnSource, no refresher, no check).
	if err := checkVulnProvider(fileCfg); err != nil {
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
	// EmbedModel is intentionally NOT required here: it only matters when
	// the elected embedder is Ollama (VESKA_EMBEDDER=ollama). With the
	// default model2vec/static election the daemon needs no Ollama embed
	// model to boot — elect.pick supplies the Ollama default if chosen.

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

	// Shared ingestion-busy predicate (solov2-181 + 8ga). Both the
	// post-promotion queue poller AND the embedder worker consult this
	// to hold their writes off the db while a cold-scan or startup
	// resync is committing. The scanTracker is constructed here (no
	// deps) so it can be captured by closures defined later; resyncRef
	// is filled in once NewStartupResync runs. Returns true when ANY
	// ingestion-side write is in flight.
	scanTracker := application.NewScanTracker()
	var resyncRef *application.StartupResync
	ingestionBusy := func() bool {
		if scanTracker.IsAnyScanRunning() {
			return true
		}
		if resyncRef != nil && resyncRef.IsSyncing() {
			return true
		}
		return false
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

	// AddedLines seam: resolve a promoted commit's newly-added lines by
	// parsing `git diff` for the repo's working tree. Keeps Promoter free
	// of an infrastructure import — git.AddedLinesForCommit is injected.
	promoter.SetAddedLinesFunc(func(ctx context.Context, repoID, gitSHA string) (map[string][]application.Line, error) {
		root, err := repoRootFunc(pools.ReadDB)(ctx, repoID)
		if err != nil {
			return nil, err
		}
		raw, err := gitwatch.AddedLinesForCommit(ctx, root, gitSHA)
		if err != nil {
			return nil, err
		}
		out := make(map[string][]application.Line, len(raw))
		for path, lines := range raw {
			al := make([]application.Line, len(lines))
			for i, l := range lines {
				al[i] = application.Line{Number: l.Number, Text: l.Text}
			}
			out[path] = al
		}
		return out, nil
	})

	checkReg := checks.NewRegistry()
	deadcodeRepo := sqlite.NewDeadCodeRepo(pools.ReadDB)
	contractRepo := sqlite.NewContractDriftRepo(pools.ReadDB)
	checkReg.Register(checks.NewDeadCodeCheck(deadcodeRepo))
	checkReg.Register(checks.NewContractDriftCheck(contractRepo))

	// Secrets-scan check (M7). Unlike vuln-scan it ships on by default — the
	// builtin scanner has no required dependency. A [promotion] config entry
	// listing "secrets-scan" in disabled_checks suppresses its registration.
	if !fileCfg.Promotion.CheckDisabled("secrets-scan") {
		secretsCheck := checks.NewSecretsScanCheck(secretsscanner.New())
		checkReg.Register(secretsCheck)
	}

	// Vulnerability-scan feature (M7). Off by default: an absent [vuln_source]
	// section yields the NullVulnSource, registers no vulnscan check, and
	// starts no refresher. provider="osv" builds the OSV adapter, registers
	// the VulnScanCheck alongside the structural checks, and arms the
	// refresher goroutine launched in Start.
	vulnSource, vulnEnabled := buildVulnSource(fileCfg)
	var vulnRefresher *vulnrefresh.Refresher
	if vulnEnabled {
		vulnRoot := func(ctx context.Context, repoID string) (string, error) {
			return repoRootFunc(pools.ReadDB)(ctx, repoID)
		}
		checkReg.Register(checks.NewVulnScanCheck(vulnSource, vulnRoot))

		var refreshOpts []vulnrefresh.Option
		if iv := vulnRefreshInterval(fileCfg); iv > 0 {
			refreshOpts = append(refreshOpts, vulnrefresh.WithInterval(iv))
		}
		refresher, rerr := vulnrefresh.NewRefresher(vulnSource, refreshOpts...)
		if rerr != nil {
			_ = pools.Close()
			return nil, fmt.Errorf("daemon: vuln refresher: %w", rerr)
		}
		vulnRefresher = refresher
	}

	runner := checks.NewRunner(checkReg, findings, metrics)
	promoter.SetCheckRunner(checkRunnerAdapter{inner: runner})

	// Embedding provider + embedder worker. When tracing is enabled the raw
	// provider is wrapped in an InstrumentedEmbedder so every Embed call emits
	// an "embed.run" span; the wrapped provider is used everywhere downstream
	// (embedder worker, MCP search tools).
	// Embedder election (solov2-1az): pick exactly ONE embedder for this
	// boot — model2vec if installed, else the in-binary static embedder;
	// Ollama only when VESKA_EMBEDDER=ollama. Vectors from different models
	// occupy incompatible spaces and must never be mixed, so this replaces
	// the old Ollama→static composite chain (solov2-soc), which did mix.
	var provider ports.EmbeddingProvider
	election, err := elect.Elect(elect.Config{
		VeskaHome:     cfg.VeskaHome,
		Override:      os.Getenv("VESKA_EMBEDDER"),
		Model2VecName: "potion-code-16M",
		OllamaURL:     cfg.OllamaURL,
		EmbedModel:    cfg.EmbedModel,
	})
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: embedder election: %w", err)
	}
	slog.Info("daemon: embedder elected", "model_id", election.Name)
	if election.SwitchedModel {
		// The elected embedder differs from what the index was last built
		// with. Wipe the content-addressed embedding store and flip every
		// ref back to pending so the embedder worker re-embeds all
		// promoted nodes under the new model. Runs synchronously here,
		// before the sqlite-vec store rehydrates, so the in-memory store
		// starts empty and search is consistent at first tick
		// (solov2-fz8). Old-model vectors are never readable again, which
		// is correct — they occupy an incompatible space.
		n, rqErr := embedder.RequeueAllUnderNewModel(context.Background(), pools.WriteEmbed)
		if rqErr != nil {
			_ = pools.Close()
			return nil, fmt.Errorf("daemon: requeue embeddings after model switch: %w", rqErr)
		}
		slog.Info("daemon: embedder changed since last boot; queued background re-embed under new model",
			"previous", election.Previous, "current", election.Name, "nodes_pending", n)
	}
	provider = election.Provider
	if tracerProvider != nil {
		provider = observability.NewInstrumentedEmbedder(provider, tracerProvider)
	}
	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.WriteEmbed)
	embedWorker, err := embedder.NewWorker(refs, provider, vec,
		embedder.WithRatePerSec(fileCfg.Embedder.RatePerSec),
		embedder.WithMaxAttempts(embedder.DefaultMaxAttempts),
		embedder.WithMetrics(metrics),
		embedder.WithPauser(ingestionBusy),
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
		wikiGraph.LoadGraph, wikiEdges.InboundEdges, wikiFindings.OpenFindingNodeIDs,
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

	// Startup-resync wiring (solov2-0z1.2). The cold-scan reparser walks a
	// repo's working tree honouring .veskaignore (loaded via the fs adapter
	// below) and feeds it into Ingester.Save + Promoter.Promote. It is
	// shared with the cold-scan-aware repoRegistrar (solov2-0z1.3) so that
	// a newly-registered repo is indexed without a daemon restart.
	ignoreAdapter := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	gitQ := gitwatch.Querier{}
	// scanTracker + resyncRef + ingestionBusy are declared earlier (just
	// after pools open) so embedder.WithPauser can share the same
	// predicate; the queue poller plugs into it here.
	poller.Pauser = ingestionBusy
	reparser, err := application.NewColdScanReparser(
		ingester, promoter, gitQ,
		application.WithIgnoreLoader(ignoreAdapter),
		application.WithScanTracker(scanTracker),
	)
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: build cold-scan reparser: %w", err)
	}

	// Cold-scan-aware repo registrar (solov2-0z1.3). eng_add_repo /
	// `veska repo add` route through this; on a successful repo.Add the
	// registrar fires a background reparser run so a newly-registered repo
	// is fully indexed without a daemon restart. daemonCtx is bound in
	// Start (Daemon.ctx is unset at construction time); scanWG drains
	// in-flight scans during Stop.
	scanWG := &sync.WaitGroup{}
	regSvc := &repoRegistrar{
		db:        pools.WriteHot,
		reparser:  reparser,
		recordFor: lookupAppRecord(pools.ReadDB),
		watchAdd:  watcher.Add,
		scanWG:    scanWG,
		// daemonCtx is bound in Start once d.ctx exists.
	}

	// MCP server. The Registry implements mcp.Handler.
	registry := mcp.NewRegistry()

	// Savings telemetry: best-effort. A failure to open the JSONL file
	// (read-only home dir, etc.) logs and continues with recording
	// disabled — telemetry is never load-bearing for search itself.
	savingsRec, err := savings.NewRecorder(filepath.Join(cfg.VeskaHome, "savings.jsonl"))
	if err != nil {
		slog.Warn("savings: recorder disabled", "err", err)
		savingsRec = nil
	}

	registerMCPTools(registry, mcpDeps{
		pools:       pools,
		cfg:         cfg,
		staging:     staging,
		vectors:     vec,
		provider:    provider,
		refs:        refs,
		metrics:     metrics,
		ingester:    ingester,
		promoter:    promoter,
		regSvc:      regSvc,
		scanTracker: scanTracker,
		savings:     savingsRec,
	})
	mcpsrv := mcp.NewServer(cfg.CLISockPath, cfg.MCPSockPath, registry)

	// Thread the TracerProvider into every tracing-aware consumer. When
	// tracing is disabled tracerProvider is nil and the consumers keep their
	// noop providers, so no spans are emitted. The InstrumentedEmbedder is
	// wired by construction above (it wraps the provider rather than exposing
	// a setter).
	if tracerProvider != nil {
		ingester.SetTracerProvider(tracerProvider)
		promoter.SetTracerProvider(tracerProvider)
		registry.SetTracerProvider(tracerProvider)
	}

	// StartupResync wiring (solov2-0z1.2). Shares the reparser closure
	// constructed above with the cold-scan-aware repoRegistrar so the
	// same code path handles both startup scans and post-registration
	// scans.
	resync := application.NewStartupResync(
		&repoLister{db: pools.ReadDB}, gitQ, ingester, promoter, reparser,
	)
	resyncRef = resync

	d := &Daemon{
		cfg:            cfg,
		pools:          pools,
		vectors:        vec,
		staging:        staging,
		gate:           gate,
		ingester:       ingester,
		promoter:       promoter,
		embed:          embedWorker,
		poller:         poller,
		watcher:        watcher,
		mcpsrv:         mcpsrv,
		mcpReg:         registry,
		metrics:        metrics,
		metricsReg:     metricsReg,
		metricsListen:  metricsListen,
		tracerProvider: tracerProvider,
		savingsRec:     savingsRec,
		vulnRefresher:  vulnRefresher,
		resync:         resync,
		regSvc:         regSvc,
		scanWG:         scanWG,
	}
	return d, nil
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
	var added map[string][]checks.Line
	if in.AddedLines != nil {
		added = make(map[string][]checks.Line, len(in.AddedLines))
		for path, lines := range in.AddedLines {
			cl := make([]checks.Line, len(lines))
			for i, l := range lines {
				cl[i] = checks.Line{Number: l.Number, Text: l.Text}
			}
			added[path] = cl
		}
	}
	a.inner.Run(ctx, checks.Input{
		RepoID:     in.RepoID,
		Branch:     in.Branch,
		GitSHA:     in.GitSHA,
		FilePaths:  in.FilePaths,
		AddedLines: added,
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
	// savings records per-search token-savings telemetry (solov2-3bu).
	// Nil disables recording — RegisterSearchTools is nil-safe.
	savings *savings.Recorder
	// ingester + promoter drive eng_promote (post-commit hook target,
	// solov2-3vv). When either is nil eng_promote is skipped at wire time.
	ingester *application.Ingester
	promoter *application.Promoter
	// regSvc is the live cold-scan-aware repoRegistrar (solov2-0z1.3).
	// When nil (legacy / test callers that don't drive registration) a
	// fallback registrar with no cold-scan dispatch is wired so the MCP
	// tool surface still functions.
	regSvc *repoRegistrar
	// scanTracker surfaces in-flight cold scans to eng_get_status
	// (solov2-pm5). Nil-safe — statusProvider tolerates a nil tracker.
	scanTracker *application.ScanTracker
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
	reg := d.regSvc
	if reg == nil {
		reg = &repoRegistrar{db: pools.WriteHot}
	}
	mcp.RegisterRepoTools(r, reg)

	// eng_promote (solov2-3vv): post-commit hook target. Requires ingester
	// + promoter + a GitQuerier — when any are missing we skip registration
	// so the tool surface degrades cleanly rather than panicking at startup.
	if d.ingester != nil && d.promoter != nil {
		mcp.RegisterPromoteTool(r, mcp.PromoteDeps{
			Repos:    &repoLister{db: pools.ReadDB},
			Git:      gitwatch.Querier{},
			Ingester: d.ingester,
			Promoter: d.promoter,
		})
	}
	// Task tools (eng_set_active_task / get_active_task / get_task_history)
	// are PARKED (solov2-6m1). There's no MCP-side path to create a task —
	// set_active_task requires the row to already exist, and no external
	// integration (Jira / Linear / GitHub) currently populates the table.
	// Exposing these tools surfaces dead-end UX (every user attempt fails
	// with 'task not found'). When a backend lands, re-enable here.
	//
	// mcp.RegisterTaskTools(r, pools.WriteHot, nil)
	_ = mcp.RegisterTaskTools // keep the symbol reachable for the future re-enable
	mcp.RegisterOwnerTools(r, pools.WriteHot)
	mcp.RegisterTodoTools(r, sqlite.NewTodoQuerierRepo(pools.ReadDB), &repoLister{db: pools.ReadDB})
	// Admin tools: repo listing + live status/config from the read pool and
	// the resolved daemon Config.
	mcp.RegisterAdminTools(r,
		&repoLister{db: pools.ReadDB},
		&statusProvider{db: pools.ReadDB, scans: d.scanTracker},
		&configProvider{cfg: d.cfg},
	)

	// Graph tools backed by the SQLite GraphRepo adapter. Writes take the
	// hot-write pool; reads take the read pool. The cross-repo resolver
	// turns cross_repo_edge_stubs into synthetic ResolvedEdges for
	// call_chain (and blast_radius below); without it the stub producer in
	// xc51.3 has no consumer (solov2-1gj).
	graph := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	resolveStubs := func(ctx context.Context, nodeID, branch string, expand bool) ([]ports.ResolvedEdge, error) {
		return resolver.ResolveStubsForNode(ctx, pools.ReadDB, nodeID, branch, expand)
	}
	mcp.RegisterGraphTools(r, graph, d.staging,
		mcp.WithRepoLister(&repoLister{db: pools.ReadDB}),
		mcp.WithResolveFunc(resolveStubs),
	)

	// Blast-radius tools. The Service walks edge adjacency + staging; the
	// repoRoot lookup resolves a repoID to its working tree, and changedFiles
	// is the git HEAD diff.
	edges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	blastSvc := blastradius.NewService(edges, nodes, d.staging)
	mcp.RegisterBlastTools(r, blastSvc, repoRootFunc(pools.ReadDB), gitwatch.ChangedFiles, &repoLister{db: pools.ReadDB},
		mcp.WithBlastResolveFunc(resolveStubs))

	// eng_find_changed_symbols: parses each file changed between two git
	// refs at both refs and diffs the symbol sets. It reads git + the
	// tree-sitter parser on demand and never touches the promoted graph,
	// so it needs no per-commit history substrate.
	if csSvc, err := changedsymbols.NewService(
		treesitter.NewGoParser(), gitwatch.ChangedFilesBetween, gitwatch.FileAtRef,
	); err == nil {
		mcp.RegisterChangedSymbolsTool(r, csSvc, repoRootFunc(pools.ReadDB), &repoLister{db: pools.ReadDB})
	} else {
		mcp.RegisterChangedSymbolsTool(r, nil, nil, &repoLister{db: pools.ReadDB})
	}

	// Wiki hot_zone surface. Change frequency comes from the git commit-history
	// reader over the default look-back window; blast radius reuses blastSvc.
	hotZoneCounts := func(ctx context.Context, repoRoot string) (map[string]int, error) {
		return gitwatch.ChangeCounts(ctx, repoRoot, 0)
	}
	if hotZoneSvc, err := wiki.NewHotZoneService(hotZoneCounts, nodes.NodesInFile, blastSvc); err == nil {
		mcp.RegisterWikiTools(r, hotZoneSvc, repoRootFunc(pools.ReadDB), &repoLister{db: pools.ReadDB})
	} else {
		mcp.RegisterWikiTools(r, nil, nil, &repoLister{db: pools.ReadDB})
	}

	// Wiki entry_points surface. Candidates are enumerated from the loaded
	// graph; the three safety gates draw on edge adjacency, blast radius and
	// the findings table.
	graphForEP := sqlite.NewGraphRepo(pools.ReadDB, pools.WriteHot)
	findingQuerier := sqlite.NewFindingQuerierRepo(pools.ReadDB)
	if epSvc, err := wiki.NewEntryPointsService(
		graphForEP.LoadGraph, edges.InboundEdges, findingQuerier.OpenFindingNodeIDs,
	); err == nil {
		mcp.RegisterEntryPointsTool(r, epSvc, &repoLister{db: pools.ReadDB})
	} else {
		mcp.RegisterEntryPointsTool(r, nil, &repoLister{db: pools.ReadDB})
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
		mcp.RegisterContextPackTool(r, cpAsm, repoRootFunc(pools.ReadDB), &repoLister{db: pools.ReadDB})
	} else {
		mcp.RegisterContextPackTool(r, nil, nil, &repoLister{db: pools.ReadDB})
	}

	// Semantic-search tools. The Service orchestrates embed → vector search →
	// node hydration with lexical fallback.
	searchSvc := search.NewService(d.provider, d.vectors, nodes,
		search.WithMetrics(d.metrics))
	mcp.RegisterSearchTools(r, searchSvc, d.refs, d.vectors, nodes, d.savings, &repoLister{db: pools.ReadDB})
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
		d.resyncDone = make(chan struct{})

		// Bind the cold-scan registrar's daemonCtx now that it exists.
		// Any AddRepo invoked before Start falls back to context.Background
		// (see repoRegistrar.AddRepo); after Start the dispatched scan is
		// tied to the daemon's lifetime context so Stop's cancel reaches it.
		if d.regSvc != nil {
			d.regSvc.daemonCtx = d.ctx
		}

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

		// Rehydrate VectorStorage from the durable node_embeddings table
		// (solov2-249). sqlite-vec is in-memory only; without this step a
		// daemon restart would leave the vector store empty until a content
		// change forces re-embedding, and semantic search would silently
		// return ≤ 0 hits. Run synchronously before the embedder worker
		// starts so a query landing in the first tick after Start sees a
		// consistent store.
		if counts, err := embedder.RehydrateVectors(d.ctx, d.pools.ReadDB, d.vectors); err != nil {
			slog.Error("daemon: rehydrate vector store", "err", err)
		} else if total := sumCounts(counts); total > 0 {
			slog.Info("daemon: rehydrated vectors", "rows", total, "buckets", len(counts))
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

		// Startup resync (solov2-0z1.2). Runs in its own goroutine so a
		// long cold-scan over a large repo cannot block Start from
		// returning — the epic constraint explicitly forbids that. The
		// goroutine respects ctx cancellation; ctx.Canceled on shutdown
		// is the expected exit path and not logged as an error.
		go func() {
			defer close(d.resyncDone)
			if d.resync == nil {
				return
			}
			if err := d.resync.Run(d.ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("daemon: startup resync", "err", err)
			}
		}()

		// OSV advisory-cache refresher. Run blocks until d.ctx is cancelled,
		// so it owns its own goroutine on the daemon's lifetime context.
		// Non-nil only when [vuln_source] provider="osv".
		if d.vulnRefresher != nil {
			go d.vulnRefresher.Run(d.ctx)
		}
	})
	return startErr
}

// sumCounts returns the total row count across all buckets — used to gate
// the "rehydrated vectors" log line to non-zero hydrates so a fresh install
// doesn't emit a misleading "rehydrated 0" message.
func sumCounts(counts map[string]int) int {
	t := 0
	for _, n := range counts {
		t += n
	}
	return t
}

// runWatchLoop reads from the multi-repo watcher and forwards each file event
// to Ingester.Save. The loop terminates when ctx is cancelled or Events()
// closes.
//
// Branch resolution (solov2-7c4): we look up each event's repo via repo.Get
// to use its recorded active_branch instead of the previous hardcoded "main".
// A non-main repo would otherwise have its live edits silently saved under
// the wrong branch key, never to be promoted (Promoter.Promote would scan an
// empty staging slice for the actual branch). A small per-event cache keeps
// the lookup cost off the hot path.
func (d *Daemon) runWatchLoop(ctx context.Context) {
	events := d.watcher.Events()
	branchOf := make(map[string]string)

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
				slog.Debug("watch loop: read failed",
					"repo_id", ev.RepoID, "path", ev.Event.Path, "err", err)
				continue
			}
			branch, ok := branchOf[ev.RepoID]
			if !ok {
				rec, gerr := repo.Get(ctx, d.pools.ReadDB, ev.RepoID)
				if gerr != nil {
					slog.Warn("watch loop: lookup repo failed",
						"repo_id", ev.RepoID, "err", gerr)
					continue
				}
				branch = rec.ActiveBranch
				if branch == "" {
					branch = "main"
				}
				branchOf[ev.RepoID] = branch
			}
			d.ingester.Save(ctx, ev.RepoID, branch, ev.Event.Path, data)
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
		if d.resyncDone != nil {
			select {
			case <-d.resyncDone:
			case <-timeout.C:
			}
		}

		// Drain in-flight AddRepo cold-scan goroutines (solov2-0z1.3).
		// Use the same 5s budget as the other background workers; ctx
		// has already been cancelled so a well-behaved reparser exits
		// promptly. A stuck scan does not wedge shutdown — we fall
		// through after the deadline and proceed with pool close.
		if d.scanWG != nil {
			scanDone := make(chan struct{})
			go func() {
				d.scanWG.Wait()
				close(scanDone)
			}()
			select {
			case <-scanDone:
			case <-timeout.C:
			}
		}

		// Shut the metrics HTTP listener down gracefully so its serve
		// goroutine exits and the port is released for the next start.
		if d.metricsCloser != nil {
			if err := d.metricsCloser.Close(); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("daemon: close metrics listener: %w", err))
			}
		}

		// Shut the OTLP TracerProvider down: this flushes any batched spans
		// and closes the exporter's gRPC connection so no goroutine leaks.
		// A bounded context keeps a stuck collector from wedging shutdown.
		if d.tracerProvider != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := d.tracerProvider.Shutdown(shutdownCtx); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("daemon: shutdown tracer provider: %w", err))
			}
			cancel()
		}

		if d.started && d.embed != nil {
			d.embed.Stop()
		}
		if d.started && d.poller != nil {
			d.poller.Wait()
		}

		if err := d.savingsRec.Close(); err != nil {
			stopErr = errors.Join(stopErr, fmt.Errorf("daemon: close savings recorder: %w", err))
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
