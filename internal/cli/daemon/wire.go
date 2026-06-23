// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

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
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
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
// All fields are optional in the sense that newDaemon will fall back to
// environment-backed defaults when zero. The composition root validates each
// resolved value and returns ErrMissingDep when a field that ultimately must
// be non-empty (e.g. SQLitePath) cannot be derived.
type Config struct {
	// VeskaHome is the data root (defaults to config.DefaultVectorDir).
	VeskaHome string

	// SQLitePath is the location of veska.db. Defaults to <VeskaHome>/veska.db.
	SQLitePath string

	// CLISockPath / MCPSockPath are the Unix sockets for the JSON-RPC server.
	// Defaults to config.CLISockPath and config.MCPSockPath.
	CLISockPath string
	MCPSockPath string

	// VectorBackend selects the VectorStorage implementation.
	// Defaults to env VESKA_VECTOR_BACKEND, then BackendMemory.
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
			c.VectorBackend = vector.BackendMemory
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
	// Ollama-embedding was the daemon default (it isn't - see elect). When
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

	staging  *staging.Area
	gate     *staging.Gate
	ingester *application.Ingester
	promoter *application.Promoter

	embed   *embedder.Worker
	poller  *queue.Poller
	watcher *gitwatch.MultiRepoWatcher
	// reconciler detects suspend/resume gaps (wall-clock jump) and re-sweeps
	// every registered repo's working tree, feeding changed files back through
	// the watcher's event stream so they re-parse exactly as a live save would.
	reconciler *gitwatch.WakeReconciler
	mcpsrv     *mcp.Server
	mcpReg     *mcp.Registry

	// resync is the startup-resync orchestrator: on Start it walks every
	// registered repo and either replays missed commits or full-reparses
	// (never-promoted / divergent SHA). Its Run is launched in its own
	// goroutine so it never blocks Start; Stop waits on resyncDone.
	resync *application.StartupResync

	// vulnScanCheck is the registered post-promotion vulnerability check
	// (non-nil iff [vuln_source] is enabled). Captured here so the
	// on-first-refresh-ok callback can run it against every registered repo
	// once the OSV cache becomes hot.
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
	// <VeskaHome>/savings.jsonl. Nil disables recording.
	// Closed on Stop so the underlying file handle is released.
	savingsRec *savings.Recorder

	startOnce sync.Once
	stopOnce  sync.Once
	started   bool
	ctx       context.Context
	cancel    context.CancelFunc
	mcpDone   chan struct{}
	wDone     chan struct{}
	// recDone is closed when the wake-reconciler tick loop returns.
	recDone chan struct{}
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
// The returned Daemon is not yet running - call Start.
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
	// single deferred guard replaces the repeated `_ = pools.Close` that
	// peppered the original monolith.
	ok := false
	defer func() {
		if !ok {
			_ = b.pools.Close()
		}
	}()

	for _, phase := range []func() error{
		// buildCheckPipeline runs before buildCore: it builds the finding
		// storage and the post-promotion check runner, which buildCore then
		// passes into the Ingester/Promoter as construction options.
		b.buildCheckPipeline,
		b.buildCore,
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
	staging  *staging.Area
	vectors  ports.VectorStorage
	provider ports.EmbeddingProvider
	refs     *sqlite.EmbeddingRefsRepo
	metrics  *observability.Metrics
	// savings records per-search token-savings telemetry.
	// Nil disables recording - RegisterSearchTools is nil-safe.
	savings *savings.Recorder
	// ingester + promoter drive eng_promote (post-commit hook target,
	// ). When either is nil eng_promote is skipped at wire time.
	ingester *application.Ingester
	promoter *application.Promoter
	// regSvc is the live cold-scan-aware repoRegistrar.
	// When nil (legacy / test callers that don't drive registration) a
	// fallback registrar with no cold-scan dispatch is wired so the MCP
	// tool surface still functions.
	regSvc *repoRegistrar
	// reparser is the cold-scan closure shared with regSvc and StartupResync.
	// Routed to eng_reindex_repo so `veska reindex` can dispatch
	// the scan in-daemon instead of needing the daemon stopped. Nil when not
	// wired (legacy / test callers); the tool degrades cleanly in that case.
	reparser func(ctx context.Context, rec application.RepoRecord) error
	// scanTracker surfaces in-flight cold scans to eng_get_status
	// Nil-safe - statusProvider tolerates a nil tracker.
	scanTracker *application.ScanTracker
	// memPressure reports whether the deferrable queue lanes are currently
	// throttled for low memory, surfaced in eng_get_status. Nil-safe.
	memPressure func() bool
	// reconciler surfaces in-flight per-repo wake sweeps so graph read tools
	// can attach a wake_reconciling degraded reason. It
	// satisfies mcp.ReconcileReader. Nil-safe - the helper no-ops on nil.
	reconciler *gitwatch.WakeReconciler
	// hubDegreeThreshold is the operator-configured blast.hub_degree_threshold
	// threaded into the blast-radius service so the gate is
	// tunable per repository layout.
	hubDegreeThreshold int
}

// repoRootFunc adapts the canonical composition.RepoRootByID resolver to
// mcp.RepoRootFunc. The underlying signatures match, so this is a trivial type
// conversion - the lookup body and error wording live in composition.
func repoRootFunc(db *sql.DB) mcp.RepoRootFunc {
	return mcp.RepoRootFunc(composition.RepoRootByID(db))
}
