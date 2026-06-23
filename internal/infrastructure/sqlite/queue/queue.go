// SPDX-License-Identifier: AGPL-3.0-only

// Package queue implements the post-promotion work queue poller.
// It manages per-work_kind goroutines that poll the post_promotion_queue table
// at a configurable cadence and dispatch rows to registered WorkHandler implementations.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/pollloop"
)

// deferBackoffSecs is how long a deferred row stays unavailable before the
// poller reconsiders it. One second keeps re-check CPU negligible while staying
// well under the tens-of-seconds embed drain it waits on; finer granularity
// buys nothing since available_at is stored in whole Unix seconds.
const deferBackoffSecs = 1

// WorkKind / WorkKind* / Row / WorkHandler are aliases of the canonical
// types defined in the ports layer. They live here too for backwards
// compatibility with the call sites that originally imported the queue
// package directly; new code should prefer the ports-layer names.
type WorkKind = ports.WorkKind

const (
	WorkKindEmbed      = ports.WorkKindEmbed
	WorkKindAutoLink   = ports.WorkKindAutoLink
	WorkKindRevalidate = ports.WorkKindRevalidate
	WorkKindReview     = ports.WorkKindReview
	WorkKindWiki       = ports.WorkKindWiki
)

// Row is the historical name for ports.WorkRow.
type Row = ports.WorkRow

// WorkHandler is the historical name for ports.WorkHandler.
type WorkHandler = ports.WorkHandler

// Poller polls the post_promotion_queue table and dispatches rows to handlers.
// One goroutine runs per registered WorkKind.
type Poller struct {
	readDB   *sql.DB
	writeDB  *sql.DB
	handlers map[WorkKind]WorkHandler
	interval time.Duration
	done     chan struct{}

	// pauser, when set and returning true, makes runKind skip its tick
	// without consuming a row. Wired to the daemon's ScanTracker so the
	// post-promotion queue yields the Write lock while a cold scan
	// is in flight. When nil the poller never pauses
	// inject via WithPauser at construction.
	pauser func() bool

	// kindPausers holds optional per-work_kind gates checked in ADDITION to
	// the global pauser. A kind whose gate returns true skips its tick (rows
	// stay pending, not failed) until the gate clears. Used to hold the
	// auto_link lane until embeddings are ready - autolink reads vectors that
	// the embedder is still producing, and a row that runs too early silently
	// under-links with no retry. Read-only after construction.
	kindPausers map[WorkKind]func() bool
}

// defaultPollInterval is the poll cadence used when WithInterval is not given.
const defaultPollInterval = 250 * time.Millisecond

// Option configures a Poller at construction.
type Option func(*Poller)

// WithInterval sets the poll interval (primarily for testing / config tuning).
// A non-positive duration is ignored, leaving the default.
func WithInterval(d time.Duration) Option {
	return func(p *Poller) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithPauser injects the pause predicate: when set and returning true, the
// poll loop skips its tick without consuming a row (e.g. to yield the Write
// lock to an in-flight cold scan). A nil predicate is ignored (never pauses).
func WithPauser(fn func() bool) Option {
	return func(p *Poller) {
		if fn != nil {
			p.pauser = fn
		}
	}
}

// WithKindPauser registers an additional pause predicate scoped to one
// work_kind, checked alongside the global pauser. A nil predicate is ignored.
func WithKindPauser(kind WorkKind, fn func() bool) Option {
	return func(p *Poller) {
		if fn == nil {
			return
		}
		if p.kindPausers == nil {
			p.kindPausers = make(map[WorkKind]func() bool)
		}
		p.kindPausers[kind] = fn
	}
}

// New creates a Poller. The poll interval defaults to 250ms; override it with
// WithInterval.
func New(readDB, writeDB *sql.DB, handlers map[WorkKind]WorkHandler, opts ...Option) *Poller {
	p := &Poller{
		readDB:   readDB,
		writeDB:  writeDB,
		handlers: handlers,
		interval: defaultPollInterval,
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Start launches one goroutine per registered WorkKind and returns immediately.
// Goroutines stop when ctx is canceled. Call Wait to block until all have exited.
func (p *Poller) Start(ctx context.Context) {
	remaining := len(p.handlers)
	if remaining == 0 {
		close(p.done)
		return
	}

	// done is closed after all per-kind goroutines exit.
	// We use a counting approach: each goroutine signals on a shared channel.
	exited := make(chan struct{}, remaining)

	for kind, handler := range p.handlers {
		go func(k WorkKind, h WorkHandler) {
			defer func() { exited <- struct{}{} }()
			p.runKind(ctx, k, h)
		}(kind, handler)
	}

	go func() {
		for range remaining {
			<-exited
		}
		close(p.done)
	}()
}

// Wait blocks until all goroutines started by Start have exited.
func (p *Poller) Wait() {
	<-p.done
}

// runKind is the per-work_kind poll loop. It drains rows greedily - back to
// back while any remain - falling back to the idle interval only once a tick
// finds nothing. The previous one-row-per-interval cadence capped drain at
// ~1 row / interval (e.g. 844 files = ~3.5min at the 250ms default); see
// pollloop.Run for the shared rationale.
func (p *Poller) runKind(ctx context.Context, kind WorkKind, handler WorkHandler) {
	kindPause := p.kindPausers[kind]
	pollloop.Run(ctx, p.interval, func(ctx context.Context) bool {
		// Yield the whole drain while a cold scan is in flight - the
		// queue's work routinely takes the Write lock for tens-to-hundreds
		// of ms per row, and contending with a serial cold-scan promote
		// turns a 1-minute scan into a 9-minute one (pprof). Returning
		// false idles for one interval, then we re-check.
		if p.pauser != nil && p.pauser() {
			return false
		}
		// Per-kind gate: e.g. hold auto_link until embeddings are ready.
		if kindPause != nil && kindPause() {
			return false
		}
		return p.processOne(ctx, kind, handler)
	})
}

// processOne fetches one pending row for kind and processes it. It returns
// true when it processed a row (so the caller should poll again immediately),
// false when the queue was empty or an unexpected DB error stalled this tick.
// Handler errors are recorded on the row, not returned.
func (p *Poller) processOne(ctx context.Context, kind WorkKind, handler WorkHandler) bool {
	// Step 1: query next pending row.
	row, err := p.fetchPending(ctx, kind)
	if err != nil {
		slog.Warn("post-promotion queue: fetch pending failed", "work_kind", kind, "err", err)
		return false
	}
	if row == nil {
		// No pending row; nothing to do.
		return false
	}

	// Step 2: CAS transition pending → in_progress.
	res, err := p.writeDB.ExecContext(ctx,
		`UPDATE post_promotion_queue SET state='in_progress', attempts=attempts+1 WHERE seq=? AND state='pending'`,
		row.Seq,
	)
	if err != nil {
		slog.Warn("post-promotion queue: claim row failed", "work_kind", kind, "seq", row.Seq, "err", err)
		return false
	}
	affected, err := res.RowsAffected()
	if err != nil {
		slog.Warn("post-promotion queue: rows-affected failed", "work_kind", kind, "seq", row.Seq, "err", err)
		return false
	}
	if affected == 0 {
		// Another goroutine grabbed it (shouldn't happen with one goroutine per kind).
		return false
	}

	// Re-read attempts from DB after increment so our local Row reflects reality.
	row.Attempts++

	// Step 3: call handler.
	handlerErr := handler.Handle(ctx, *row)

	now := time.Now().Unix()
	if errors.Is(handlerErr, ports.ErrDeferWork) {
		// Precondition not met (not a failure): hold the row until available_at
		// and undo the attempt the CAS just charged, so a row waiting on a slow
		// precondition never exhausts its retry budget. The lane keeps draining
		// other ready rows in the meantime.
		if _, err = p.writeDB.ExecContext(ctx,
			`UPDATE post_promotion_queue SET state='pending', attempts=attempts-1, available_at=? WHERE seq=?`,
			now+deferBackoffSecs, row.Seq,
		); err != nil {
			slog.Warn("post-promotion queue: defer row failed", "work_kind", kind, "seq", row.Seq, "err", err)
		}
		return true
	}
	if handlerErr == nil {
		// Step 4a: success.
		if _, err = p.writeDB.ExecContext(ctx,
			`UPDATE post_promotion_queue SET state='done', completed_at=? WHERE seq=?`,
			now, row.Seq,
		); err != nil {
			slog.Warn("post-promotion queue: mark done failed", "work_kind", kind, "seq", row.Seq, "err", err)
		}
		// A row was processed regardless of the bookkeeping outcome - poll again.
		return true
	}

	// Step 4b: failure - re-queue or fail permanently.
	if row.Attempts >= 3 {
		_, err = p.writeDB.ExecContext(ctx,
			`UPDATE post_promotion_queue SET state='failed', error=? WHERE seq=?`,
			handlerErr.Error(), row.Seq,
		)
	} else {
		_, err = p.writeDB.ExecContext(ctx,
			`UPDATE post_promotion_queue SET state='pending', error=? WHERE seq=?`,
			handlerErr.Error(), row.Seq,
		)
	}
	if err != nil {
		slog.Warn("post-promotion queue: record handler failure failed", "work_kind", kind, "seq", row.Seq, "err", err)
	}
	return true
}

// fetchPending queries the next pending row for the given work_kind.
// Returns nil, nil when no row is available.
func (p *Poller) fetchPending(ctx context.Context, kind WorkKind) (*Row, error) {
	r := &Row{}
	err := p.readDB.QueryRowContext(ctx, `
		SELECT seq, promotion_id, repo_id, branch, git_sha, work_kind, payload, state, attempts, enqueued_at
		FROM post_promotion_queue
		WHERE state='pending' AND work_kind=? AND available_at<=?
		ORDER BY seq
		LIMIT 1`,
		string(kind), time.Now().Unix(),
	).Scan(
		&r.Seq, &r.PromotionID, &r.RepoID, &r.Branch, &r.GitSHA,
		&r.Kind, &r.Payload, &r.State, &r.Attempts, &r.EnqueuedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
