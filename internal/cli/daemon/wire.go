package daemon

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
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/contextpack"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/vulnrefresh"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
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

	// vulnScanCheck is the registered post-promotion vulnerability check
	// (non-nil iff [vuln_source] is enabled). Captured here so the
	// on-first-refresh-ok callback can run it against every registered repo
	// once the OSV cache becomes hot (solov2-jtl5.4).
	vulnScanCheck *checks.VulnScanCheck
	// findings is the FindingStorage handle used by the post-commit check
	// runner. Captured here so the same persistence path is reused when
	// scanAllReposForVuln runs synthetic checks outside the promote flow.
	findings ports.FindingStorage

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
//
// The work is split across daemonBuilder phase methods so each stays small, and
// the partial-failure cleanup (closing the SQLite pools once they are open) is
// expressed once as a deferred guard rather than repeated at every error site.
func newDaemon(cfg Config) (*Daemon, error) {
	b := &daemonBuilder{cfg: ResolveConfig(cfg)}

	// Pre-storage phases hold no closable resources, so a failure here just
	// returns the error.
	for _, phase := range []func() error{
		b.loadConfig,
		b.buildObservability,
		b.validateConfig,
	} {
		if err := phase(); err != nil {
			return nil, err
		}
	}

	if err := b.openStorage(); err != nil {
		return nil, err
	}
	// The SQLite pools are now open. Every later failure must close them; a
	// single deferred guard replaces the repeated `_ = pools.Close()` that
	// peppered the original monolith.
	ok := false
	defer func() {
		if !ok {
			_ = b.pools.Close()
		}
	}()

	for _, phase := range []func() error{
		b.buildCore,
		b.buildCheckPipeline,
		b.buildEmbedder,
		b.buildQueueHandlers,
		b.buildPollerWatcher,
		b.buildMCPServer,
		b.finalize,
	} {
		if err := phase(); err != nil {
			return nil, err
		}
	}

	d := b.assemble()
	ok = true
	return d, nil
}

// daemonBuilder accumulates the daemon's collaborator graph across phase
// methods. Most fields map 1:1 to Daemon fields (assemble copies them); the
// rest are intermediates shared between phases (fileCfg, the ingestion-busy
// predicate and its scanTracker/resyncRef, provider/refs/reparser, handlers).
type daemonBuilder struct {
	cfg     Config
	fileCfg config.Config

	metrics       *observability.Metrics
	metricsReg    *prometheus.Registry
	metricsListen string
	tracer        *sdktrace.TracerProvider

	pools *sqlite.Pools
	vec   ports.VectorStorage

	staging  *application.StagingArea
	gate     *application.IngestionGate
	ingester *application.Ingester
	promoter *application.Promoter
	findings ports.FindingStorage

	scanTracker   *application.ScanTracker
	resyncRef     *application.StartupResync
	ingestionBusy func() bool

	vulnRefresher *vulnrefresh.Refresher
	vulnScanCheck *checks.VulnScanCheck

	provider    ports.EmbeddingProvider
	refs        *sqlite.EmbeddingRefsRepo
	embedWorker *embedder.Worker

	handlers map[queue.WorkKind]queue.WorkHandler
	poller   *queue.Poller
	watcher  *gitwatch.MultiRepoWatcher
	reparser func(ctx context.Context, rec application.RepoRecord) error
	regSvc   *repoRegistrar
	scanWG   *sync.WaitGroup

	registry   *mcp.Registry
	mcpsrv     *mcp.Server
	savingsRec *savings.Recorder
	resync     *application.StartupResync
}

// loadConfig loads ~/.veska/config.toml (defaults < config.toml < env vars). A
// missing file is not an error; see docs/operations/CONFIG-SURFACE.md.
func (b *daemonBuilder) loadConfig() error {
	fileCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("daemon: load config: %w", err)
	}
	b.fileCfg = fileCfg
	return nil
}

// buildObservability constructs the Prometheus metric set and the OTLP
// TracerProvider, each only when enabled. The both-or-neither tracing rule
// (enabled<->endpoint) is a fatal startup error so operator intent is never
// silently ignored. config.Validate covers the file surface; this re-check also
// covers the test overrides.
func (b *daemonBuilder) buildObservability() error {
	metricsEnabled := b.fileCfg.Metrics.Enabled || b.cfg.MetricsEnabled
	b.metricsListen = b.fileCfg.Metrics.Listen
	if b.cfg.MetricsListen != "" {
		b.metricsListen = b.cfg.MetricsListen
	}
	if metricsEnabled {
		b.metricsReg = prometheus.NewRegistry()
		b.metrics = observability.NewMetrics(b.metricsReg)
	}

	tracingEnabled := b.fileCfg.Tracing.Enabled || b.cfg.TracingEnabled
	tracingEndpoint := b.fileCfg.Tracing.OTLPEndpoint
	if b.cfg.TracingEndpoint != "" {
		tracingEndpoint = b.cfg.TracingEndpoint
	}
	if tracingEnabled && tracingEndpoint == "" {
		return &ErrMissingDep{Name: "tracing.otlp_endpoint",
			Why: "tracing is enabled but no OTLP endpoint is set (set tracing.otlp_endpoint or VESKA_OTLP_ENDPOINT)"}
	}
	if !tracingEnabled && tracingEndpoint != "" {
		return &ErrMissingDep{Name: "tracing.enabled",
			Why: "an OTLP endpoint is set but tracing is disabled (set tracing.enabled = true or clear the endpoint)"}
	}
	if tracingEnabled {
		tp, err := observability.NewTracerProvider(tracingEndpoint)
		if err != nil {
			return fmt.Errorf("daemon: construct tracer provider: %w", err)
		}
		b.tracer = tp
	}
	return nil
}

// validateConfig fails fast on misconfiguration before any resource is opened:
// the review LLM provider, the vuln advisory provider, the vector backend, the
// required socket/db paths, and creation of the SQLite parent directory.
// EmbedModel is intentionally NOT required — it only matters when the elected
// embedder is Ollama (VESKA_EMBEDDER=ollama).
func (b *daemonBuilder) validateConfig() error {
	if err := checkLLMProvider(b.fileCfg); err != nil {
		return err
	}
	if err := checkVulnProvider(b.fileCfg); err != nil {
		return err
	}
	switch b.cfg.VectorBackend {
	case vector.BackendSQLiteVec, vector.BackendUsearch:
	default:
		return &ErrMissingDep{Name: "vector_backend",
			Why: fmt.Sprintf("unknown VESKA_VECTOR_BACKEND %q (want %q or %q)",
				b.cfg.VectorBackend, vector.BackendSQLiteVec, vector.BackendUsearch)}
	}
	if b.cfg.SQLitePath == "" {
		return &ErrMissingDep{Name: "sqlite_path"}
	}
	if b.cfg.CLISockPath == "" {
		return &ErrMissingDep{Name: "cli_sock_path"}
	}
	if b.cfg.MCPSockPath == "" {
		return &ErrMissingDep{Name: "mcp_sock_path"}
	}
	if err := os.MkdirAll(filepath.Dir(b.cfg.SQLitePath), 0o755); err != nil {
		return fmt.Errorf("daemon: mkdir sqlite dir: %w", err)
	}
	return nil
}

// openStorage opens the SQLite pools, applies migrations, builds the shared
// ingestion-busy predicate, and opens the vector backend. It closes the pools
// itself on a post-open failure because the caller installs its deferred
// pools-close guard only after openStorage returns successfully.
func (b *daemonBuilder) openStorage() error {
	pools, err := sqlite.OpenPools(b.cfg.SQLitePath)
	if err != nil {
		return fmt.Errorf("daemon: open sqlite pools: %w", err)
	}
	b.pools = pools
	if _, err := sqlite.OpenWithOptions(b.cfg.SQLitePath, sqlite.Options{}); err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: migrate sqlite: %w", err)
	}

	// Shared ingestion-busy predicate (solov2-181 + 8ga): the queue poller and
	// the embedder worker both hold writes off while a cold-scan or startup
	// resync is committing. resyncRef is filled in by finalize; the closure
	// reads it through the builder so the later assignment is visible.
	b.scanTracker = application.NewScanTracker()
	b.ingestionBusy = func() bool {
		if b.scanTracker.IsAnyScanRunning() {
			return true
		}
		if b.resyncRef != nil && b.resyncRef.IsSyncing() {
			return true
		}
		return false
	}

	vec, err := vector.NewVectorStorage(b.cfg.VectorBackend, b.cfg.VeskaHome)
	if err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: open vector storage: %w", err)
	}
	b.vec = vec
	return nil
}

// buildCore wires the shared ingestion+promotion core (internal/composition),
// the finding storage, and the git-diff AddedLines seam.
func (b *daemonBuilder) buildCore() error {
	core := composition.NewColdScanCore(b.pools, b.fileCfg.Review.Enabled)
	b.staging = core.Staging
	b.gate = core.Gate
	b.ingester = core.Ingester
	b.promoter = core.Promoter

	b.findings = sqlite.NewFindingRepo(b.pools.Write)
	b.ingester.SetFindingStorage(b.findings)
	b.promoter.SetAddedLinesFunc(composition.GitAddedLinesFunc(repoRootFunc(b.pools.ReadDB)))
	return nil
}

// buildCheckPipeline registers the post-promotion structural checks (dead-code,
// contract-drift, secrets-scan, and the optional vuln-scan) and installs the
// check runner on the promoter.
func (b *daemonBuilder) buildCheckPipeline() error {
	checkReg := checks.NewRegistry()
	deadcodeRepo := sqlite.NewDeadCodeRepo(b.pools.ReadDB)
	contractRepo := sqlite.NewContractDriftRepo(b.pools.ReadDB)
	// solov2-izh6.13: dead-code skips ephemeral (cache-tier) repos cloned by
	// `veska search --repo <url>`, mirroring the autolink short-circuit.
	deadcodeRepoKind := func(ctx context.Context, repoID string) (string, error) {
		rec, err := repo.Get(ctx, b.pools.ReadDB, repoID)
		if err != nil {
			return "", err
		}
		return rec.Kind, nil
	}
	checkReg.Register(checks.NewDeadCodeCheck(deadcodeRepo,
		checks.WithDeadCodeRepoKindLookup(deadcodeRepoKind),
	))
	checkReg.Register(checks.NewContractDriftCheck(contractRepo))

	// Secrets-scan ships on by default (no required dependency); a [promotion]
	// disabled_checks entry listing "secrets-scan" suppresses it.
	if !b.fileCfg.Promotion.CheckDisabled("secrets-scan") {
		checkReg.Register(checks.NewSecretsScanCheck(secretsscanner.New()))
	}

	if err := b.registerVulnCheck(checkReg); err != nil {
		return err
	}

	runner := checks.NewRunner(checkReg, b.findings, b.metrics)
	b.promoter.SetCheckRunner(composition.CheckRunnerAdapter{Inner: runner})
	return nil
}

// registerVulnCheck arms the vulnerability-scan feature when [vuln_source]
// provider="osv": it registers the VulnScanCheck and builds the advisory-cache
// refresher (launched later in Start). Off by default — an absent section
// yields NullVulnSource, no check, and no refresher.
func (b *daemonBuilder) registerVulnCheck(checkReg *checks.Registry) error {
	vulnSource, vulnEnabled := buildVulnSource(b.fileCfg)
	if !vulnEnabled {
		return nil
	}
	vulnRoot := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(b.pools.ReadDB)(ctx, repoID)
	}
	b.vulnScanCheck = checks.NewVulnScanCheck(vulnSource, vulnRoot)
	checkReg.Register(b.vulnScanCheck)

	var refreshOpts []vulnrefresh.Option
	if iv := vulnRefreshInterval(b.fileCfg); iv > 0 {
		refreshOpts = append(refreshOpts, vulnrefresh.WithInterval(iv))
	}
	refresher, err := vulnrefresh.NewRefresher(vulnSource, refreshOpts...)
	if err != nil {
		return fmt.Errorf("daemon: vuln refresher: %w", err)
	}
	b.vulnRefresher = refresher
	return nil
}

// buildEmbedder elects exactly one embedder for this boot and constructs the
// embedder worker.
func (b *daemonBuilder) buildEmbedder() error {
	if err := b.electEmbedder(); err != nil {
		return err
	}
	b.refs = sqlite.NewEmbeddingRefsRepo(b.pools.ReadDB, b.pools.Write)
	worker, err := embedder.NewWorker(b.refs, b.provider, b.vec,
		embedder.WithRatePerSec(b.fileCfg.Embedder.RatePerSec),
		embedder.WithMaxAttempts(embedder.DefaultMaxAttempts),
		embedder.WithMetrics(b.metrics),
		embedder.WithPauser(b.ingestionBusy),
	)
	if err != nil {
		return fmt.Errorf("daemon: embedder worker: %w", err)
	}
	b.embedWorker = worker
	return nil
}

// electEmbedder picks the single embedder for this boot (model2vec if
// installed, else the in-binary static embedder; Ollama only when
// VESKA_EMBEDDER=ollama). Vectors from different models occupy incompatible
// spaces (solov2-soc), so a model switch wipes the embedding store and
// re-queues every promoted node under the new model (solov2-fz8).
func (b *daemonBuilder) electEmbedder() error {
	election, err := elect.Elect(elect.Config{
		VeskaHome:     b.cfg.VeskaHome,
		Override:      os.Getenv("VESKA_EMBEDDER"),
		Model2VecName: "potion-code-16M",
		OllamaURL:     b.cfg.OllamaURL,
		EmbedModel:    b.cfg.EmbedModel,
	})
	if err != nil {
		return fmt.Errorf("daemon: embedder election: %w", err)
	}
	slog.Info("daemon: embedder elected", "model_id", election.Name)
	// solov2-yql1: surface a one-shot WARN so operators tailing daemon.log see
	// why eng_search_semantic returns 'low_quality_static_embedder'.
	if election.Name == "veska-static-v2" {
		slog.Warn("daemon: low-quality static-v2 embedder elected — run `veska install model2vec` for higher-quality code search",
			"model_id", election.Name)
	}
	if election.SwitchedModel {
		n, rqErr := embedder.RequeueAllUnderNewModel(context.Background(), b.pools.Write)
		if rqErr != nil {
			return fmt.Errorf("daemon: requeue embeddings after model switch: %w", rqErr)
		}
		slog.Info("daemon: embedder changed since last boot; queued background re-embed under new model",
			"previous", election.Previous, "current", election.Name, "nodes_pending", n)
	}
	provider := election.Provider
	if b.tracer != nil {
		provider = observability.NewInstrumentedEmbedder(provider, b.tracer)
	}
	b.provider = provider
	return nil
}

// buildQueueHandlers builds the post-promotion work handlers (autolink,
// revalidate, wiki, the no-op embed drain, and the optional review lane) into
// the handlers map consumed by the poller.
func (b *daemonBuilder) buildQueueHandlers() error {
	autoH, err := b.buildAutolinkHandler()
	if err != nil {
		return err
	}
	revalH := revalidate.NewHandler(sqlite.NewRevalidateRepo(b.pools.Write), revalidate.WithMetrics(b.metrics))
	wikiH, err := b.buildWikiHandler()
	if err != nil {
		return err
	}
	b.handlers = map[queue.WorkKind]queue.WorkHandler{
		ports.WorkKindAutoLink:   autoH,
		ports.WorkKindRevalidate: revalH,
		ports.WorkKindWiki:       wikiH,
		ports.WorkKindEmbed:      noopEmbedHandler{}, // drained by embed worker
	}
	if b.fileCfg.Review.Enabled {
		reviewH, rerr := b.buildReviewHandler()
		if rerr != nil {
			return rerr
		}
		b.handlers[ports.WorkKindReview] = reviewH
	}
	return nil
}

// buildAutolinkHandler wires the SIMILAR_TO autolink handler. solov2-izh6.8:
// the repo-kind lookup skips ephemeral (cache-tier) repos.
func (b *daemonBuilder) buildAutolinkHandler() (*autolink.Handler, error) {
	nodeLookup := sqlite.NewNodeLookupRepo(b.pools.ReadDB)
	edgeRepo := sqlite.NewEdgeRepo(b.pools.Write)
	linker, err := autolink.NewLinker(b.refs, b.vec, autolink.WithMetrics(b.metrics))
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
	)
	if err != nil {
		return nil, fmt.Errorf("daemon: autolink handler: %w", err)
	}
	return autoH, nil
}

// buildWikiHandler wires the WorkKindWiki regeneration handler (hot_zone +
// entry_points pages) via the shared composition constructor. The daemon shares
// its live staging so blast radius sees in-flight nodes, resolves repo roots
// through the repos table, and honours the [wiki] write_pages config.
func (b *daemonBuilder) buildWikiHandler() (*wiki.Handler, error) {
	return composition.NewWikiHandler(b.pools, b.staging, repoRootFunc(b.pools.ReadDB), b.fileCfg.Wiki.WritePages)
}

// buildReviewHandler wires the optional WorkKindReview lane: the Ollama
// generator, prompt loader, per-commit/per-day token quota (persisted in
// daemon_state), and the audit writer. Only called when review is enabled.
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
		b.fileCfg.LLMGenerator.Endpoint, b.fileCfg.LLMGenerator.Model, nil, genOpts...)
	reviewRoot := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(b.pools.ReadDB)(ctx, repoID)
	}
	// Token-quota enforcement (solov2-nz2.5): the per-day total persists in
	// daemon_state; the audit writer records the daily-cap pause.
	tokenStore := sqlite.NewReviewTokenStore(b.pools.ReadDB, b.pools.Write)
	quota := review.NewQuota(
		b.fileCfg.Review.MaxTokensPerCommit,
		b.fileCfg.Review.MaxTokensPerDay,
		tokenStore, nil)
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

// buildPollerWatcher constructs the post-promotion queue poller, the fsnotify
// watcher, the shared cold-scan reparser, and the cold-scan-aware repo
// registrar (solov2-0z1.2/0z1.3). The poller and embedder share ingestionBusy.
func (b *daemonBuilder) buildPollerWatcher() error {
	pollInterval := 250 * time.Millisecond
	if d, derr := time.ParseDuration(b.fileCfg.PostPromotionQueue.PollInterval); derr == nil && d > 0 {
		pollInterval = d
	}
	b.poller = queue.NewWithInterval(b.pools.ReadDB, b.pools.Write, b.handlers, pollInterval)
	b.poller.Pauser = b.ingestionBusy
	b.watcher = gitwatch.NewMultiRepoWatcher()

	ignoreAdapter := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := application.NewColdScanReparser(
		b.ingester, b.promoter, gitwatch.Querier{},
		application.WithIgnoreLoader(ignoreAdapter),
		application.WithScanTracker(b.scanTracker),
	)
	if err != nil {
		return fmt.Errorf("daemon: build cold-scan reparser: %w", err)
	}
	b.reparser = reparser

	b.scanWG = &sync.WaitGroup{}
	b.regSvc = &repoRegistrar{
		db:        b.pools.Write,
		reparser:  reparser,
		recordFor: lookupAppRecord(b.pools.ReadDB),
		watchAdd:  b.watcher.Add,
		scanWG:    b.scanWG,
		// daemonCtx is bound in Start once d.ctx exists.
	}
	return nil
}

// buildMCPServer builds the MCP registry, opens the best-effort savings
// recorder, registers every tool family, and constructs the MCP socket server.
func (b *daemonBuilder) buildMCPServer() error {
	b.registry = mcp.NewRegistry()

	// Savings telemetry is best-effort: a failure to open the JSONL file logs
	// and continues with recording disabled — never load-bearing for search.
	rec, err := savings.NewRecorder(filepath.Join(b.cfg.VeskaHome, "savings.jsonl"))
	if err != nil {
		slog.Warn("savings: recorder disabled", "err", err)
		rec = nil
	}
	b.savingsRec = rec

	registerMCPTools(b.registry, mcpDeps{
		pools:       b.pools,
		cfg:         b.cfg,
		staging:     b.staging,
		vectors:     b.vec,
		provider:    b.provider,
		refs:        b.refs,
		metrics:     b.metrics,
		ingester:    b.ingester,
		promoter:    b.promoter,
		regSvc:      b.regSvc,
		reparser:    b.reparser,
		scanTracker: b.scanTracker,
		savings:     b.savingsRec,
	})
	b.mcpsrv = mcp.NewServer(b.cfg.CLISockPath, b.cfg.MCPSockPath, b.registry)
	return nil
}

// finalize threads the TracerProvider into the tracing-aware consumers (a no-op
// when tracing is disabled) and wires the startup-resync orchestrator, sharing
// the reparser closure with the repo registrar (solov2-0z1.2).
func (b *daemonBuilder) finalize() error {
	if b.tracer != nil {
		b.ingester.SetTracerProvider(b.tracer)
		b.promoter.SetTracerProvider(b.tracer)
		b.registry.SetTracerProvider(b.tracer)
	}
	resync := application.NewStartupResync(
		&repoLister{db: b.pools.ReadDB}, gitwatch.Querier{}, b.ingester, b.promoter, b.reparser,
	)
	b.resync = resync
	b.resyncRef = resync
	return nil
}

// assemble builds the Daemon from the populated builder. It cannot fail; the
// caller marks success so the deferred pools-close guard is disarmed.
func (b *daemonBuilder) assemble() *Daemon {
	return &Daemon{
		cfg:            b.cfg,
		pools:          b.pools,
		vectors:        b.vec,
		staging:        b.staging,
		gate:           b.gate,
		ingester:       b.ingester,
		promoter:       b.promoter,
		embed:          b.embedWorker,
		poller:         b.poller,
		watcher:        b.watcher,
		mcpsrv:         b.mcpsrv,
		mcpReg:         b.registry,
		metrics:        b.metrics,
		metricsReg:     b.metricsReg,
		metricsListen:  b.metricsListen,
		tracerProvider: b.tracer,
		savingsRec:     b.savingsRec,
		vulnRefresher:  b.vulnRefresher,
		vulnScanCheck:  b.vulnScanCheck,
		findings:       b.findings,
		resync:         b.resync,
		regSvc:         b.regSvc,
		scanWG:         b.scanWG,
	}
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
	// reparser is the cold-scan closure shared with regSvc and StartupResync.
	// Routed to eng_reindex_repo (solov2-4d7b) so `veska reindex` can dispatch
	// the scan in-daemon instead of needing the daemon stopped. Nil when not
	// wired (legacy / test callers); the tool degrades cleanly in that case.
	reparser func(ctx context.Context, rec application.RepoRecord) error
	// scanTracker surfaces in-flight cold scans to eng_get_status
	// (solov2-pm5). Nil-safe — statusProvider tolerates a nil tracker.
	scanTracker *application.ScanTracker
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
	d.startOnce.Do(func() {
		d.started = true
		d.ctx, d.cancel = context.WithCancel(ctx)
		d.mcpDone = make(chan struct{})
		d.wDone = make(chan struct{})
		d.resyncDone = make(chan struct{})

		// Bind the cold-scan registrar's daemonCtx now that it exists. Any
		// AddRepo invoked before Start falls back to context.Background (see
		// repoRegistrar.AddRepo); after Start the dispatched scan is tied to the
		// daemon's lifetime context so Stop's cancel reaches it.
		if d.regSvc != nil {
			d.regSvc.daemonCtx = d.ctx
		}

		d.startMetricsListener()
		d.startMCPServer()
		d.awaitListenerSockets()
		d.rehydrateVectors()

		d.embed.Start(d.ctx)
		d.poller.Start(d.ctx)
		d.watcher.Start(d.ctx)
		d.seedWatcher()

		d.startWatchLoop()
		d.startResync()
		d.startVulnRefresher()
	})
	return nil
}

// startMetricsListener binds the Prometheus metrics HTTP listener when metrics
// are enabled; its closer is shut down in Stop. A bind failure is logged, not
// fatal — a daemon without a metrics endpoint is still a valid running daemon.
func (d *Daemon) startMetricsListener() {
	if d.metrics == nil {
		return
	}
	closer, addr, err := observability.StartHTTPListener(d.metricsListen, d.metricsReg)
	if err != nil {
		slog.Error("daemon: metrics listener", "addr", d.metricsListen, "err", err)
		return
	}
	d.metricsCloser = closer
	d.metricsAddr = addr
	slog.Info("daemon: metrics listener bound", "addr", addr)
}

// startMCPServer launches the MCP server in its own goroutine; its Start blocks
// until d.ctx is done. mcpDone is closed when the server goroutine exits.
func (d *Daemon) startMCPServer() {
	go func() {
		defer close(d.mcpDone)
		if err := d.mcpsrv.Start(d.ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("daemon: mcp server exited", "err", err)
		}
	}()
}

// awaitListenerSockets waits briefly for the listener sockets to appear so
// callers can rely on them being present after Start returns. The MCP server
// creates them synchronously inside Start, so the 500ms ceiling is ample.
func (d *Daemon) awaitListenerSockets() {
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, errCLI := os.Stat(d.cfg.CLISockPath)
		_, errMCP := os.Stat(d.cfg.MCPSockPath)
		if errCLI == nil && errMCP == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rehydrateVectors repopulates the in-memory VectorStorage from the durable
// node_embeddings table (solov2-249). Run synchronously before the embedder
// worker starts so a query landing in the first tick after Start sees a
// consistent store; without it a restart leaves search returning ≤ 0 hits until
// a content change forces re-embedding.
func (d *Daemon) rehydrateVectors() {
	if counts, err := embedder.RehydrateVectors(d.ctx, d.pools.ReadDB, d.vectors); err != nil {
		slog.Error("daemon: rehydrate vector store", "err", err)
	} else if total := sumCounts(counts); total > 0 {
		slog.Info("daemon: rehydrated vectors", "rows", total, "buckets", len(counts))
	}
}

// seedWatcher registers every known repository with the fsnotify watcher. A
// failed or empty listing is logged, not fatal — a daemon watching zero repos
// is still a valid running daemon.
func (d *Daemon) seedWatcher() {
	repos, err := repo.List(d.ctx, d.pools.ReadDB)
	if err != nil {
		slog.Error("daemon: list repos for watcher", "err", err)
		return
	}
	for _, r := range repos {
		if err := d.watcher.Add(r.RepoID, r.RootPath); err != nil {
			slog.Error("daemon: watch repo", "repo", r.RepoID, "err", err)
		}
	}
}

// startWatchLoop bridges filesystem events → Ingester.Save in its own
// goroutine; wDone is closed when the loop returns.
func (d *Daemon) startWatchLoop() {
	go func() {
		defer close(d.wDone)
		d.runWatchLoop(d.ctx)
	}()
}

// startResync runs the startup resync (solov2-0z1.2) in its own goroutine so a
// long cold-scan over a large repo cannot block Start from returning. ctx
// cancellation on shutdown is the expected exit and not logged as an error.
func (d *Daemon) startResync() {
	go func() {
		defer close(d.resyncDone)
		if d.resync == nil {
			return
		}
		if err := d.resync.Run(d.ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("daemon: startup resync", "err", err)
		}
	}()
}

// startVulnRefresher launches the OSV advisory-cache refresher (non-nil only
// when [vuln_source] provider="osv"); its Run blocks until d.ctx is cancelled.
// On the first successful refresh it kicks a one-shot vuln-scan sweep over every
// repo so promotions that ran against a cold cache get retroactive findings
// (solov2-jtl5.4).
func (d *Daemon) startVulnRefresher() {
	if d.vulnRefresher == nil {
		return
	}
	d.vulnRefresher.SetOnFirstRefreshOk(d.scanAllReposForVuln)
	go d.vulnRefresher.Run(d.ctx)
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
		// One bounded budget shared across the background-goroutine and
		// cold-scan drains: a stuck goroutine cannot wedge shutdown forever,
		// and we still close the pool so the next start isn't blocked on a
		// stale lock. The single timer is passed to both drains so the 5s is a
		// shared ceiling, not 5s each.
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()

		d.awaitBackgroundGoroutines(timeout.C)
		d.drainScans(timeout.C)
		stopErr = d.closeResources()

		// Best-effort socket cleanup (MCP server already removes them, but
		// belt-and-braces if it crashed before reaching its defer).
		_ = os.Remove(d.cfg.CLISockPath)
		_ = os.Remove(d.cfg.MCPSockPath)
	})
	return stopErr
}

// awaitBackgroundGoroutines waits for the MCP server, watch loop, and startup
// resync to exit. mcpDone and resyncDone share the caller's bounded budget;
// wDone gets its own short 500ms wait (the watch loop exits promptly on cancel).
func (d *Daemon) awaitBackgroundGoroutines(timeoutC <-chan time.Time) {
	if d.mcpDone != nil {
		select {
		case <-d.mcpDone:
		case <-timeoutC:
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
		case <-timeoutC:
		}
	}
}

// drainScans waits for in-flight AddRepo cold-scan goroutines (solov2-0z1.3)
// under the shared budget. ctx is already cancelled so a well-behaved reparser
// exits promptly; a stuck scan does not wedge shutdown — we fall through after
// the deadline and proceed with pool close.
func (d *Daemon) drainScans(timeoutC <-chan time.Time) {
	if d.scanWG == nil {
		return
	}
	scanDone := make(chan struct{})
	go func() {
		d.scanWG.Wait()
		close(scanDone)
	}()
	select {
	case <-scanDone:
	case <-timeoutC:
	}
}

// closeResources shuts down the metrics listener, tracer provider, embedder,
// poller, savings recorder, and SQLite pools, joining every close error so a
// single failure doesn't mask the rest.
func (d *Daemon) closeResources() error {
	var err error
	// Metrics HTTP listener: release the port for the next start.
	if d.metricsCloser != nil {
		if cerr := d.metricsCloser.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("daemon: close metrics listener: %w", cerr))
		}
	}
	// OTLP TracerProvider: flush batched spans + close the exporter so no
	// goroutine leaks; a bounded context keeps a stuck collector from wedging.
	if d.tracerProvider != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if serr := d.tracerProvider.Shutdown(shutdownCtx); serr != nil {
			err = errors.Join(err, fmt.Errorf("daemon: shutdown tracer provider: %w", serr))
		}
		cancel()
	}
	if d.started && d.embed != nil {
		d.embed.Stop()
	}
	if d.started && d.poller != nil {
		d.poller.Wait()
	}
	if cerr := d.savingsRec.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("daemon: close savings recorder: %w", cerr))
	}
	if d.pools != nil {
		if cerr := d.pools.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("daemon: close pools: %w", cerr))
		}
	}
	return err
}
