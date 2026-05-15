package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/autolink"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/revalidate"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/queue"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
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

	// Promoter + structural check pipeline.
	promoter := application.NewPromoter(staging, pools.WriteHot)
	checkReg := checks.NewRegistry()
	deadcodeRepo := sqlite.NewDeadCodeRepo(pools.ReadDB)
	contractRepo := sqlite.NewContractDriftRepo(pools.ReadDB)
	checkReg.Register(checks.NewDeadCodeCheck(deadcodeRepo))
	checkReg.Register(checks.NewContractDriftCheck(contractRepo))
	runner := checks.NewRunner(checkReg, findings, nil)
	promoter.SetCheckRunner(checkRunnerAdapter{inner: runner})

	// Embedding provider + embedder worker.
	provider, err := ollama.New(cfg.EmbedModel, ollama.WithBaseURL(cfg.OllamaURL))
	if err != nil {
		_ = pools.Close()
		return nil, fmt.Errorf("daemon: embedding provider: %w", err)
	}
	refs := sqlite.NewEmbeddingRefsRepo(pools.ReadDB, pools.WriteEmbed)
	embedWorker, err := embedder.NewWorker(refs, provider, vec,
		embedder.WithRatePerSec(embedder.DefaultRatePerSec),
		embedder.WithMaxAttempts(embedder.DefaultMaxAttempts),
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
	linker, err := autolink.NewLinker(refs, vec)
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
	revalH := revalidate.NewHandler(revalRepo)

	handlers := map[queue.WorkKind]queue.WorkHandler{
		ports.WorkKindAutoLink:   autoH,
		ports.WorkKindRevalidate: revalH,
		ports.WorkKindEmbed:      noopEmbedHandler{}, // drained by embed worker
	}
	poller := queue.New(pools.ReadDB, pools.WriteHot, handlers)

	// fsnotify multi-repo watcher.
	watcher := gitwatch.NewMultiRepoWatcher()

	// MCP server. The Registry implements mcp.Handler.
	registry := mcp.NewRegistry()
	registerMCPTools(registry, pools)
	mcpsrv := mcp.NewServer(cfg.CLISockPath, cfg.MCPSockPath, registry)

	return &Daemon{
		cfg:      cfg,
		pools:    pools,
		vectors:  vec,
		staging:  staging,
		gate:     gate,
		ingester: ingester,
		promoter: promoter,
		embed:    embedWorker,
		poller:   poller,
		watcher:  watcher,
		mcpsrv:   mcpsrv,
	}, nil
}

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

// registerMCPTools wires every available tool family into the registry.
// Tool families that require collaborators we have not built yet (e.g. a
// ports.GraphStorage implementation) are skipped — a missing tool surfaces
// as MethodNotFound rather than as a daemon-start failure.
func registerMCPTools(r *mcp.Registry, pools *sqlite.Pools) {
	// Tools that only need *sql.DB + AuditWriter.
	mcp.RegisterFindingTools(r, pools.WriteHot, nil)
	mcp.RegisterSuppressionTools(r, pools.WriteHot, nil)
	mcp.RegisterTaskTools(r, pools.WriteHot, nil)
	mcp.RegisterOwnerTools(r, pools.WriteHot)
	mcp.RegisterTodoTools(r, sqlite.NewTodoQuerierRepo(pools.ReadDB))
	// RegisterAdminTools is omitted — it requires application.RepoLister /
	// mcp.StatusProvider / mcp.ConfigProvider implementations that have no
	// production adapter yet (see follow-up beads filed alongside this wiring).
	// NOTE: RegisterGraphTools / RegisterBlastTools / RegisterSearchTools are
	// intentionally omitted — they require collaborators (GraphStorage,
	// blastradius.Service, etc.) that have no production adapter yet. See
	// follow-up beads filed alongside this wiring.
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
