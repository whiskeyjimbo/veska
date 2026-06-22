// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"fmt"
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
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/fts"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/application/summary"
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
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

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

	staging     *staging.Area
	gate        *staging.Gate
	ingester    *application.Ingester
	promoter    *application.Promoter
	findings    ports.FindingStorage
	checkRunner application.CheckRunner

	scanTracker      *application.ScanTracker
	resyncRef        *application.StartupResync
	ingestionBusy    func() bool
	availMem         availMemFunc
	embedderIsOllama bool

	vulnRefresher *vulnrefresh.Refresher
	vulnScanCheck *checks.VulnScanCheck

	provider    ports.EmbeddingProvider
	refs        *sqlite.EmbeddingRefsRepo
	embedWorker *embedder.Worker

	handlers   map[queue.WorkKind]queue.WorkHandler
	poller     *queue.Poller
	watcher    *gitwatch.MultiRepoWatcher
	reconciler *gitwatch.WakeReconciler
	reparser   func(ctx context.Context, rec application.RepoRecord) error
	regSvc     *repoRegistrar
	scanWG     *sync.WaitGroup

	registry   *mcp.Registry
	mcpsrv     *mcp.Server
	savingsRec *savings.Recorder
	resync     *application.StartupResync
}

// loadConfig loads ~/.veska/config.toml (defaults < config.toml < env vars). A
// missing file is not an error; see
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
		return &ErrMissingDep{
			Name: "tracing.otlp_endpoint",
			Why:  "tracing is enabled but no OTLP endpoint is set (set tracing.otlp_endpoint or VESKA_OTLP_ENDPOINT)",
		}
	}
	if !tracingEnabled && tracingEndpoint != "" {
		return &ErrMissingDep{
			Name: "tracing.enabled",
			Why:  "an OTLP endpoint is set but tracing is disabled (set tracing.enabled = true or clear the endpoint)",
		}
	}
	if tracingEnabled {
		tp, err := observability.NewTracerProvider(tracingEndpoint, b.fileCfg.Tracing.SampleRatio)
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
// EmbedModel is intentionally NOT required - it only matters when the elected
// embedder is Ollama (VESKA_EMBEDDER=ollama).
func (b *daemonBuilder) validateConfig() error {
	if err := checkLLMProvider(b.fileCfg); err != nil {
		return err
	}
	if err := checkVulnProvider(b.fileCfg); err != nil {
		return err
	}
	switch b.cfg.VectorBackend {
	case vector.BackendMemory, vector.BackendUsearch:
	default:
		return &ErrMissingDep{
			Name: "vector_backend",
			Why: fmt.Sprintf("unknown VESKA_VECTOR_BACKEND %q (want %q or %q)",
				b.cfg.VectorBackend, vector.BackendMemory, vector.BackendUsearch),
		}
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
	// The daemon may start before `veska init` has run, so it is also a
	// creator of VESKA_HOME. Keep it owner-only (0700): veska.db holds parsed
	// private source and potential secrets. Chmod fixes pre-existing 0755 roots.
	dataDir := filepath.Dir(b.cfg.SQLitePath)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("daemon: mkdir sqlite dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return fmt.Errorf("daemon: secure sqlite dir: %w", err)
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
	if _, err := sqlite.OpenWithOptions(b.cfg.SQLitePath, sqlite.Options{VerifyIntegrity: b.fileCfg.Storage.VerifyMigrationIntegrity}); err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: migrate sqlite: %w", err)
	}

	b.buildIngestionBusy(defaultAvailMem)

	// One-shot startup advisory: warn when the in-memory vector backend is
	// elected on a low-RAM host, since memvec keeps every vector in RAM.
	// Mirrors the static-embedder WARN in electEmbedder.
	maybeWarnLowMemory(b.cfg.VectorBackend, b.availMem, slog.Default())

	vec, err := vector.NewVectorStorage(b.cfg.VectorBackend, b.cfg.VeskaHome)
	if err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: open vector storage: %w", err)
	}
	b.vec = vec
	return nil
}

// buildIngestionBusy installs the scan tracker and the shared ingestion-busy
// predicate: the queue poller and embedder worker hold writes off while a
// cold-scan or startup resync is committing, or while the host is under memory
// pressure (both lanes skip their tick to let RAM recover instead of risking
// OOM). resyncRef is filled in by finalize; the closure reads it through the
// builder. avail is injected so tests can drive the memory-pressure branch.
func (b *daemonBuilder) buildIngestionBusy(avail availMemFunc) {
	b.scanTracker = application.NewScanTracker()
	b.availMem = avail
	b.ingestionBusy = func() bool {
		if b.scanTracker.IsAnyScanRunning() {
			return true
		}
		if b.resyncRef != nil && b.resyncRef.IsSyncing() {
			return true
		}
		return underMemoryPressure(b.availMem)
	}
}

// buildCore wires the shared ingestion+promotion core (internal/composition),
// the finding storage, and the git-diff AddedLines seam.
func (b *daemonBuilder) buildCore() error {
	ingesterOpts := []application.IngesterOption{
		application.WithFindingStorage(b.findings),
	}
	promoterOpts := []application.PromoterOption{
		application.WithCheckRunner(b.checkRunner),
		application.WithAddedLinesFunc(composition.GitAddedLinesFunc(repoRootFunc(b.pools.ReadDB))),
	}
	// Pass the tracer only when one was constructed: wrapping a nil concrete
	// *sdktrace.TracerProvider in the option's interface param would defeat the
	// noop fallback (non-nil interface, nil concrete value).
	if b.tracer != nil {
		ingesterOpts = append(ingesterOpts, application.WithIngesterTracerProvider(b.tracer))
		promoterOpts = append(promoterOpts, application.WithPromoterTracerProvider(b.tracer))
	}

	core := composition.NewColdScanCore(b.pools, ingesterOpts, promoterOpts,
		composition.WithReviewEnabled(b.fileCfg.Review.Enabled),
		composition.WithSummaryEnabled(b.fileCfg.Summary.Enabled))
	b.staging = core.Staging
	b.gate = core.Gate
	b.ingester = core.Ingester
	b.promoter = core.Promoter
	return nil
}

// buildCheckPipeline registers the post-promotion structural checks (dead-code,
// contract-drift, secrets-scan, and the optional vuln-scan) and installs the
// check runner on the promoter.
func (b *daemonBuilder) buildCheckPipeline() error {
	b.findings = sqlite.NewFindingRepo(b.pools.Write)

	checkReg := checks.NewRegistry()
	deadcodeRepo := sqlite.NewDeadCodeRepo(b.pools.ReadDB)
	contractRepo := sqlite.NewContractDriftRepo(b.pools.ReadDB)
	// dead-code skips ephemeral (cache-tier) repos cloned by
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
	checkReg.Register(checks.NewUntestedSymbolCheck(sqlite.NewCoverageRepo(b.pools.ReadDB),
		checks.WithUntestedRepoKindLookup(deadcodeRepoKind),
		checks.WithUntestedInterfaceMethods(deadcodeRepo),
	))

	// Secrets-scan (on unless disabled) + vuln-scan (only when provider="osv")
	// share their enablement policy with the cold-scan CLI path via
	// composition.RegisterCommonChecks; the dead-code + contract-drift checks
	// above are daemon-only. The vuln source is built once here and fed to BOTH
	// the check and its advisory-cache refresher.
	vulnSource, vulnEnabled := buildVulnSource(b.fileCfg)
	vulnRoot := func(ctx context.Context, repoID string) (string, error) {
		return repoRootFunc(b.pools.ReadDB)(ctx, repoID)
	}
	b.vulnScanCheck = composition.RegisterCommonChecks(
		checkReg, b.fileCfg, vulnSource, vulnEnabled, vulnRoot,
	)
	if err := b.buildVulnRefresher(vulnSource, vulnEnabled); err != nil {
		return err
	}

	runner := checks.NewRunner(checkReg, b.findings, b.metrics, checks.WithLogger(slog.Default()))
	b.checkRunner = composition.CheckRunnerAdapter{Inner: runner}
	return nil
}

// buildVulnRefresher builds the advisory-cache refresher (launched later in
// Start) for vuln-scan. The VulnScanCheck registration lives in
// composition.RegisterCommonChecks; this is the daemon-only refresher half, and
// is absent when vuln-scan is disabled.
func (b *daemonBuilder) buildVulnRefresher(vulnSource ports.VulnSource, vulnEnabled bool) error {
	if !vulnEnabled {
		return nil
	}

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
	// Throughput is bounded by the greedy drain + Governor, not a fixed rate.
	// The default governor (fixed concurrency 1) suits a single local Ollama
	// instance and local embedders alike: both serialize internally, so 1 is
	// the ceiling and the greedy drain reaches it. Hosted-API providers will
	// elect an adaptive governor here once they land (solov2-fi42).
	worker, err := embedder.NewWorker(b.refs, b.provider, b.vec,
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
// spaces, so a model switch wipes the store and re-queues every node.
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
	// one-shot WARN so operators tailing daemon.log see why search returns
	// 'low_quality_static_embedder'.
	if election.Name == "veska-static-v2" {
		slog.Warn("daemon: low-quality static-v2 embedder elected - run `veska install model2vec` for higher-quality code search",
			"model_id", election.Name)
	}
	if election.SwitchedModel {
		archive := sqlite.NewEmbeddingArchive(b.pools.ReadDB, b.pools.Write)
		n, rqErr := archive.RequeueAllUnderNewModel(context.Background())
		if rqErr != nil {
			return fmt.Errorf("daemon: requeue embeddings after model switch: %w", rqErr)
		}
		slog.Info("daemon: embedder changed since last boot; queued background re-embed under new model",
			"previous", election.Previous, "current", election.Name, "nodes_pending", n)
	}
	b.embedderIsOllama = election.Ollama
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

// buildPollerWatcher constructs the post-promotion queue poller, the fsnotify
// watcher, the shared cold-scan reparser, and the cold-scan-aware repo
// registrar. The poller and embedder share ingestionBusy.
func (b *daemonBuilder) buildPollerWatcher() error {
	pollInterval := parseDurationOr(b.fileCfg.PostPromotionQueue.PollInterval, 250*time.Millisecond)
	// Hold the auto_link lane until the embedder has drained. autolink reads the
	// vectors the embedder produces; a row that runs while embeddings are still
	// pending silently skips its not-yet-embedded source nodes and is never
	// retried, permanently under-linking those files on a cold scan (solov2-22e8).
	// CountPending excludes orphaned refs (deleted nodes), and failed embeds end
	// in state='failed' not 'pending', so the gate always clears.
	autolinkGate := func() bool {
		n, err := b.refs.CountPending(context.Background())
		return err == nil && n > 0
	}
	b.poller = queue.New(b.pools.ReadDB, b.pools.Write, b.handlers,
		queue.WithInterval(pollInterval), queue.WithPauser(b.ingestionBusy),
		queue.WithKindPauser(ports.WorkKindAutoLink, autolinkGate))
	b.watcher = gitwatch.NewMultiRepoWatcher()
	b.reconciler = b.buildReconciler()

	ignoreAdapter := func(repoRoot string) (application.IgnoreMatcher, error) {
		return fsignore.Load(repoRoot)
	}
	reparser, err := composition.NewColdScanReparser(
		b.ingester, b.promoter, ignoreAdapter,
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
		// A repo added mid-session is registered with both the live watcher and
		// the wake reconciler so a later suspend/resume re-sweeps it too.
		watchAdd: func(repoID, rootPath string) error {
			b.reconciler.AddDir(repoID, rootPath)
			return b.watcher.Add(repoID, rootPath)
		},
		scanWG: b.scanWG,
		// daemonCtx is bound in Start once d.ctx exists.
	}
	return nil
}

// buildReconciler constructs the suspend/resume wake reconciler. Its handler
// feeds each changed file back through the watcher's event stream (Inject), so
// wake-detected changes re-parse via the same path as live fsnotify writes
// repo/branch resolution and Ingester.Save are reused verbatim in runWatchLoop.
// wake_tick / wake_threshold are read from [watcher]; bad or empty values fall
// back to the documented defaults (CONFIG-SURFACE: 5s tick, 30s threshold).
func (b *daemonBuilder) buildReconciler() *gitwatch.WakeReconciler {
	tick := parseDurationOr(b.fileCfg.Watcher.WakeTick, 5*time.Second)
	threshold := parseDurationOr(b.fileCfg.Watcher.WakeThreshold, 30*time.Second)

	opts := []gitwatch.Option{gitwatch.WithWakeConcurrency(b.fileCfg.Watcher.WakeConcurrency)}
	// Staging-vs-HEAD check: a serial pre-pass at sweep start
	// reconciles each repo's working-tree branch against repos.active_branch,
	// bumping the staging generation before any parse runs. b.gate/b.staging are
	// populated by buildCore, which runs before buildReconciler.
	if br, err := application.NewBranchReconciler(
		gitBranchReader{},
		&activeBranchStore{read: b.pools.ReadDB, write: b.pools.Write},
		b.gate, b.staging,
	); err != nil {
		slog.Warn("wake reconciler: branch reconcile disabled", "err", err)
	} else {
		opts = append(opts, gitwatch.WithSweepStartHook(
			func(ctx context.Context, repoID, dir string) {
				if _, err := br.Reconcile(ctx, repoID, dir); err != nil {
					slog.Warn("wake reconciler: branch reconcile failed", "repo", repoID, "err", err)
				}
			}))
	}

	// Post-sweep: restart every repo's watcher handle so live saves resume
	// against a fresh OS stream once the mtime sweep has covered the suspend
	// window ( step 4).
	opts = append(opts, gitwatch.WithPostSweepHook(func(_ context.Context) {
		b.watcher.RestartAll()
	}))

	// Baseline seam: the reconciler compares against the live
	// FSWatcher lastSeen map (kept current by the live save path) instead of its
	// own seeded copy, resolved fresh each sweep so it follows RestartAll.
	opts = append(opts, gitwatch.WithBaseline(b.watcher.BaselineFor))

	return gitwatch.NewWakeReconciler(tick, threshold,
		func(_ context.Context, repoID, path string) {
			b.watcher.Inject(repoID, path)
		},
		opts...)
}

// parseDurationOr returns the parsed positive duration or fallback on any
// empty / unparseable / non-positive value.
func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return fallback
}

// buildMCPServer builds the MCP registry, opens the best-effort savings
// recorder, registers every tool family, and constructs the MCP socket server.
func (b *daemonBuilder) buildMCPServer() error {
	b.registry = mcp.NewRegistry()

	// Savings telemetry is best-effort: a failure to open the JSONL file logs
	// and continues with recording disabled - never load-bearing for search.
	rec, err := savings.NewRecorder(filepath.Join(b.cfg.VeskaHome, "savings.jsonl"))
	if err != nil {
		slog.Warn("savings: recorder disabled", "err", err)
		rec = nil
	}
	b.savingsRec = rec

	if err := registerMCPTools(b.registry, mcpDeps{
		pools:              b.pools,
		cfg:                b.cfg,
		staging:            b.staging,
		vectors:            b.vec,
		provider:           b.provider,
		refs:               b.refs,
		metrics:            b.metrics,
		ingester:           b.ingester,
		promoter:           b.promoter,
		regSvc:             b.regSvc,
		reparser:           b.reparser,
		scanTracker:        b.scanTracker,
		reconciler:         b.reconciler,
		savings:            b.savingsRec,
		hubDegreeThreshold: b.fileCfg.Blast.HubDegreeThreshold,
	}); err != nil {
		return fmt.Errorf("register MCP tools: %w", err)
	}
	b.mcpsrv = mcp.NewServer(b.cfg.CLISockPath, b.cfg.MCPSockPath, b.registry)
	return nil
}

// finalize threads the TracerProvider into the tracing-aware consumers (a no-op
// when tracing is disabled) and wires the startup-resync orchestrator, sharing
// the reparser closure with the repo registrar.
func (b *daemonBuilder) finalize() error {
	// The Ingester and Promoter receive the tracer as a construction option in
	// buildCore; only the MCP registry is wired here (it is built later, by
	// buildMCPServer).
	if b.tracer != nil {
		b.registry.SetTracerProvider(b.tracer)
	}
	// Staging-vs-HEAD branch check: the same reconciler the
	// wake-sweep uses also runs at the START of each startup-resync repo, so a
	// branch switch during downtime bumps the generation and drops prior-branch
	// staging before any replay. Degrade-don't-crash: if construction fails
	// (nil dep) we log and proceed WITHOUT the option - startup must not fail
	// because the branch check couldn't wire (mirrors buildReconciler).
	var resyncOpts []application.StartupResyncOption
	if br, berr := application.NewBranchReconciler(
		gitBranchReader{},
		&activeBranchStore{read: b.pools.ReadDB, write: b.pools.Write},
		b.gate, b.staging,
	); berr != nil {
		slog.Warn("startup resync: branch reconcile disabled", "err", berr)
	} else {
		resyncOpts = append(resyncOpts, application.WithBranchReconciler(br))
	}

	resync, err := application.NewStartupResync(
		&repoLister{db: b.pools.ReadDB}, gitwatch.Querier{}, b.ingester.Save, b.promoter.Promote, b.reparser,
		resyncOpts...,
	)
	if err != nil {
		return err
	}
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
		reconciler:     b.reconciler,
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
