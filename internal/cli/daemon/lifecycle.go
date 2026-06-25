// SPDX-License-Identifier: AGPL-3.0-only

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// This file holds the Daemon runtime lifecycle: Start spawns the background
// goroutines (MCP server, embedder, poller, watcher, resync, vuln refresher),
// and Stop cancels them and releases resources under a bounded drain budget.
// The composition root that builds the Daemon (newDaemon + daemonBuilder) lives
// in wire.go.

// Start launches all background goroutines. It is safe to call multiple times
// subsequent invocations are no-ops.
func (d *Daemon) Start(ctx context.Context) error {
	d.startOnce.Do(func() {
		d.started = true
		d.ctx, d.cancel = context.WithCancel(ctx)
		d.mcpDone = make(chan struct{})
		d.wDone = make(chan struct{})
		d.recDone = make(chan struct{})
		d.resyncDone = make(chan struct{})

		// Bind the cold-scan registrar's daemonCtx now that it exists. Any
		// AddRepo invoked before Start falls back to context.Background (see
		// repoRegistrar.AddRepo); after Start the dispatched scan is tied to the
		// daemon's lifetime context so Stop's cancel reaches it.
		if d.regSvc != nil {
			d.regSvc.daemonCtx = d.ctx
		}

		d.startMetricsListener()
		// Wire the watcher's parent context before the MCP socket opens. The
		// server is served in a goroutine and immediately accepts repo-add
		// requests, whose watchAdd path calls startRepoWatch -> context.WithCancel
		// on this ctx; a scripted boot+add against a home with an existing repo
		// (slow rehydrateVectors below) otherwise races into a nil m.ctx panic.
		// Start is a pure context assignment - no goroutine, no deps on the lines
		// between here and the original site.
		d.watcher.Start(d.ctx)
		d.startMCPServer()
		d.awaitListenerSockets()
		d.rehydrateVectors()

		d.embed.Start(d.ctx)
		d.poller.Start(d.ctx)
		d.seedWatcher()
		d.startReconciler()

		d.startWatchLoop()
		d.startResync()
		d.startVulnRefresher()
	})
	return nil
}

// startMetricsListener binds the Prometheus metrics HTTP listener when metrics
// are enabled; its closer is shut down in Stop. A bind failure is logged, not
// fatal - a daemon without a metrics endpoint is still a valid running daemon.
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
// node_embeddings table. Run synchronously before the embedder
// worker starts so a query landing in the first tick after Start sees a
// consistent store; without it a restart leaves search returning ≤ 0 hits until
// a content change forces re-embedding.
func (d *Daemon) rehydrateVectors() {
	archive := sqlite.NewEmbeddingArchive(d.pools.ReadDB, d.pools.Write)
	if counts, err := embedder.RehydrateVectors(d.ctx, archive, d.vectors); err != nil {
		slog.Error("daemon: rehydrate vector store", "err", err)
	} else if total := sumCounts(counts); total > 0 {
		slog.Info("daemon: rehydrated vectors", "rows", total, "buckets", len(counts))
	}
}

// seedWatcher registers every known repository with the fsnotify watcher. A
// failed or empty listing is logged, not fatal - a daemon watching zero repos
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
		// Register the same tree with the wake reconciler so a suspend/resume
		// gap re-sweeps it; it reads the watcher's live lastSeen baseline (just
		// seeded by Add above) via the WithBaseline seam, so the first sweep
		// reports only suspend-window changes.
		d.reconciler.AddDir(r.RepoID, r.RootPath)
	}
}

// startReconciler launches the wake-reconciler tick loop in its own goroutine;
// recDone is closed when it returns (on d.ctx cancellation). No separate seed
// pass is needed: each repo is Added to the watcher in
// seedWatcher, which seeds the watcher's lastSeen baseline, and the reconciler
// reads that live baseline via the WithBaseline seam.
func (d *Daemon) startReconciler() {
	go func() {
		defer close(d.recDone)
		d.reconciler.Start(d.ctx)
	}()
}

// startWatchLoop bridges filesystem events → Ingester.Save in its own
// goroutine; wDone is closed when the loop returns.
func (d *Daemon) startWatchLoop() {
	go func() {
		defer close(d.wDone)
		d.runWatchLoop(d.ctx)
	}()
}

// startResync runs the startup resync in its own goroutine so a
// long cold-scan over a large repo cannot block Start from returning. ctx
// cancellation on shutdown is the expected exit and not logged as an error.
// The identity scheme change (migration 0019) needs no special
// handling here: 0019 drops the derived graph and clears last_promoted_sha, so
// the normal resync full-reparses every repo under the new scheme on next boot.
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
// when [vuln_source] provider="osv"); its Run blocks until d.ctx is canceled.
// On the first successful refresh it kicks a one-shot vuln-scan sweep over every
// repo so promotions that ran against a cold cache get retroactive findings
func (d *Daemon) startVulnRefresher() {
	if d.vulnRefresher == nil {
		return
	}
	d.vulnRefresher.SetOnFirstRefreshOk(d.scanAllReposForVuln)
	go d.vulnRefresher.Run(d.ctx)
}

// sumCounts returns the total row count across all buckets - used to gate
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
// to the ingester: write events to Ingester.Save, remove events to
// Ingester.DeleteFile (which stages a tombstone so promotion deletes the gone
// file's nodes/vectors). The loop terminates when ctx is canceled or Events
// closes.
// Branch resolution: we look up each event's repo via repo.Get
// to use its recorded active_branch instead of the previous hardcoded "main".
// A non-main repo would otherwise have its live edits silently saved under
// the wrong branch key, never to be promoted (Promoter.Promote would scan an
// empty staging slice for the actual branch). A small per-event cache keeps
// the lookup cost off the hot path.
func (d *Daemon) runWatchLoop(ctx context.Context) {
	events := d.watcher.Events()
	// Per-repo cache of (active_branch, root_path). root_path is needed to
	// relativise the absolute fsnotify path before it reaches the parser
	// nodeID + nodes.file_path key on the repo-relative slash
	// path, so the cold-scan and hot (fsnotify) paths must agree.
	branchOf := make(map[string]string)
	rootOf := make(map[string]string)
	// Mirror the cold-scan walk filter: only files the parser understands can
	// own nodes, so only those need a delete tombstone. An empty set (parser
	// advertises no extensions) disables the filter so deletions are never
	// silently dropped.
	supportedExt := make(map[string]struct{})
	for _, e := range d.ingester.SupportedExtensions() {
		supportedExt[strings.ToLower(e)] = struct{}{}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			// Branch/root must be resolved before the op split: a remove
			// event has no readable file, so the old "ReadFile first" order
			// dropped every deletion at the read error and never staged the
			// tombstone that deletes the gone file's nodes/vectors.
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
				rootOf[ev.RepoID] = rec.RootPath
			}
			rel, rerr := filepath.Rel(rootOf[ev.RepoID], ev.Event.Path)
			if rerr != nil {
				slog.Warn("watch loop: relativise path failed; skipping",
					"repo_id", ev.RepoID, "root", rootOf[ev.RepoID],
					"path", ev.Event.Path, "err", rerr)
				continue
			}
			relSlash := filepath.ToSlash(rel)

			if ev.Event.Op == ports.WatchOpRemove {
				_, supported := supportedExt[strings.ToLower(filepath.Ext(ev.Event.Path))]
				if supported || len(supportedExt) == 0 {
					d.ingester.DeleteFile(ctx, ev.RepoID, branch, relSlash)
				}
				continue
			}

			data, err := os.ReadFile(ev.Event.Path)
			if err != nil {
				slog.Debug("watch loop: read failed",
					"repo_id", ev.RepoID, "path", ev.Event.Path, "err", err)
				continue
			}
			d.ingester.Save(ctx, ev.RepoID, branch, relSlash, data)
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
	if d.recDone != nil {
		select {
		case <-d.recDone:
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

// drainScans waits for in-flight AddRepo cold-scan goroutines
// under the shared budget. ctx is already canceled so a well-behaved reparser
// exits promptly; a stuck scan does not wedge shutdown - we fall through after
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
