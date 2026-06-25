// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/savings"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/application/vulnrefresh"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	fsignore "github.com/whiskeyjimbo/veska/internal/infrastructure/fs"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
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
	writeBusy        func() bool
	memPressure      func() bool
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
	case vector.BackendMemory, vector.BackendUsearch, vector.BackendAuto:
	default:
		return &ErrMissingDep{
			Name: "vector_backend",
			Why: fmt.Sprintf("unknown VESKA_VECTOR_BACKEND %q (want %q, %q, or %q)",
				b.cfg.VectorBackend, vector.BackendMemory, vector.BackendUsearch, vector.BackendAuto),
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

	// Resolve BackendAuto now that the DB is migrated and queryable: elect
	// usearch when an already-indexed (repo,branch) is large enough to benefit
	// (and usearch is compiled in), else memvec. An explicit backend passes
	// through unchanged. NOTE: this is a startup decision keyed on what is
	// ALREADY indexed - a fresh repo's first cold scan still runs on memvec; the
	// election takes effect on the next boot once it crosses the threshold.
	maxRepoVec := b.maxRepoVectorCount()
	backend := vector.ElectVectorBackend(b.cfg.VectorBackend, maxRepoVec, vector.UsearchAvailable())
	if b.cfg.VectorBackend == vector.BackendAuto {
		slog.Info("daemon: vector backend auto-elected", "backend", backend,
			"max_repo_vectors", maxRepoVec, "threshold", vector.AutoElectThreshold)
	}

	// One-shot startup advisory: warn when the in-memory vector backend is
	// elected on a low-RAM host, since memvec keeps every vector in RAM.
	// Mirrors the static-embedder WARN in electEmbedder.
	maybeWarnLowMemory(backend, b.availMem, slog.Default())

	vecOpts, err := vector.OptionsForProfile(b.fileCfg.Storage.UsearchIndexProfile)
	if err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: vector storage: %w", err)
	}
	vec, err := vector.NewVectorStorage(backend, b.cfg.VeskaHome, vecOpts...)
	if err != nil && b.cfg.VectorBackend == vector.BackendAuto && backend == vector.BackendUsearch {
		// Auto-elected usearch but it failed to open (e.g. libusearch_c.so not on
		// the loader path). Auto must never brick the daemon - degrade to memvec.
		// An EXPLICIT usearch choice still hard-fails below, as before.
		slog.Warn("daemon: auto-elected usearch failed to open, falling back to memory backend",
			"err", err, "max_repo_vectors", maxRepoVec)
		backend = vector.BackendMemory
		vec, err = vector.NewVectorStorage(backend, b.cfg.VeskaHome, vecOpts...)
	}
	if err != nil {
		_ = pools.Close()
		return fmt.Errorf("daemon: open vector storage: %w", err)
	}
	// Write the resolved backend back so assemble() propagates it to the Daemon
	// and eng_get_config reports the concrete backend, not "auto".
	b.cfg.VectorBackend = backend
	b.vec = vec
	return nil
}

// maxRepoVectorCount returns the ready-vector population of the largest single
// (repo,branch) index - the value that drives memvec's per-query linear-scan
// cost and usearch's per-index build. Mirrors the rehydrate bucketing
// (LoadReadyEmbeddings). COALESCE keeps it one row (0) on an empty graph; a
// query error degrades to 0 (memvec), never blocks startup.
func (b *daemonBuilder) maxRepoVectorCount() int {
	var n int
	err := b.pools.ReadDB.QueryRow(`
		SELECT COALESCE(MAX(c), 0) FROM (
			SELECT COUNT(*) AS c
			FROM node_embedding_refs r
			JOIN nodes n           ON n.node_id = r.node_id
			JOIN node_embeddings e ON e.content_hash = r.content_hash
			WHERE r.state = 'ready' AND r.content_hash IS NOT NULL
			GROUP BY n.repo_id, n.branch
		)`).Scan(&n)
	if err != nil {
		slog.Warn("daemon: vector backend auto-elect: count failed, assuming memory", "err", err)
		return 0
	}
	return n
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
		composition.WithSummaryEnabled(b.fileCfg.Summary.Enabled),
		composition.WithVectorPruner(b.vec.DeleteNodes))
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
	// import-cycle is zero-config and repo-wide; it reuses the ephemeral-repo
	// skip so cycles in cache-tier clones aren't reported.
	checkReg.Register(checks.NewImportCycleCheck(sqlite.NewPackageDepsRepo(b.pools.ReadDB),
		checks.WithImportCycleRepoKindLookup(deadcodeRepoKind),
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
// coldScanEmbedBatchSize is the per-batch ref count for the embed drain. The
// 50k-node sweep in tools/loadtest/embedder put the throughput knee at 128
// (32->128 = 1.47x, 128->256 flat); see buildEmbedder for the rationale.
const coldScanEmbedBatchSize = 128

func (b *daemonBuilder) buildEmbedder() error {
	if err := b.electEmbedder(); err != nil {
		return err
	}
	b.refs = sqlite.NewEmbeddingRefsRepo(b.pools.ReadDB, b.pools.Write)
	// Throughput is bounded by the greedy drain + Governor, not a fixed rate.
	// Hosted-API providers will elect an adaptive governor here once they land.
	opts := []embedder.Option{
		embedder.WithMaxAttempts(embedder.DefaultMaxAttempts),
		embedder.WithMetrics(b.metrics),
		// writeBusy, not ingestionBusy: the embedder pauses only to avoid racing
		// the promotion Write tx (scan/resync), never on memory pressure -
		// pausing there caused a silent indefinite stall.
		embedder.WithPauser(b.writeBusy),
		// 128 over the 32 default: once embed compute fans across cores the drain
		// is SQL-bound, and a larger batch amortizes the per-batch write tx +
		// statement prepares. A 50k-node drain sweep showed 32->128 = 1.47x with
		// 128->256 flat, so 128 is the knee - the throughput win without holding
		// governor*256 rows resident per pass.
		embedder.WithBatchSize(coldScanEmbedBatchSize),
	}
	// The local model2vec/static embedder is pure CPU per node (tokenize ->
	// row lookup -> mean-pool -> normalize) with no internal parallelism, so the
	// default fixed-1 governor pins cold-scan embedding to a single core. Embed
	// is a read-only pure function (concurrent-safe), so fan it across the
	// available cores. Ollama keeps fixed-1: a single local instance serializes
	// internally and the API path's concurrency lever is its own RPM/TPM-sized
	// governor, not this one. GOMAXPROCS(0) (not NumCPU) so container CPU quotas
	// and explicit overrides are respected.
	if !b.embedderIsOllama {
		if n := runtime.GOMAXPROCS(0); n > 1 {
			opts = append(opts, embedder.WithGovernor(embedder.NewFixedGovernor(n)))
		}
	}
	worker, err := embedder.NewWorker(b.refs, b.provider, b.vec, opts...)
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

// buildPollerWatcher constructs the post-promotion queue poller, the fsnotify
// watcher, the shared cold-scan reparser, and the cold-scan-aware repo
// registrar. The poller and embedder share ingestionBusy.
func (b *daemonBuilder) buildPollerWatcher() error {
	pollInterval := parseDurationOr(b.fileCfg.PostPromotionQueue.PollInterval, 250*time.Millisecond)
	// The auto_link lane runs concurrently with the embed drain; correctness
	// comes from the per-file embed gate in the autolink handler
	// (WithEmbedReadiness), which defers a row until that file's own nodes are
	// embedded. This overlaps autolink with embedding instead of holding the
	// whole lane until the backlog clears.
	b.poller = queue.New(b.pools.ReadDB, b.pools.Write, b.handlers,
		queue.WithInterval(pollInterval), queue.WithPauser(b.ingestionBusy))
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
		memPressure:        b.memPressure,
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
