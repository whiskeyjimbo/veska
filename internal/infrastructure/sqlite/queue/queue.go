// Package queue implements the post-promotion work queue poller.
// It manages per-work_kind goroutines that poll the post_promotion_queue table
// at a configurable cadence and dispatch rows to registered WorkHandler implementations.
package queue

import (
	"context"
	"database/sql"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

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
}

// New creates a Poller with the default 250ms poll interval.
func New(readDB, writeDB *sql.DB, handlers map[WorkKind]WorkHandler) *Poller {
	return NewWithInterval(readDB, writeDB, handlers, 250*time.Millisecond)
}

// NewWithInterval creates a Poller with a custom poll interval (primarily for testing).
func NewWithInterval(readDB, writeDB *sql.DB, handlers map[WorkKind]WorkHandler, interval time.Duration) *Poller {
	return &Poller{
		readDB:   readDB,
		writeDB:  writeDB,
		handlers: handlers,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start launches one goroutine per registered WorkKind and returns immediately.
// Goroutines stop when ctx is cancelled. Call Wait to block until all have exited.
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

// runKind is the per-work_kind poll loop.
func (p *Poller) runKind(ctx context.Context, kind WorkKind, handler WorkHandler) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// processOne only returns errors for unexpected DB failures;
		// handler errors are handled inline. We still wait the interval.
		_ = p.processOne(ctx, kind, handler)

		timer.Reset(p.interval)
	}
}

// processOne fetches one pending row for kind and processes it.
// Returns nil when no row is available or processing completes (success or failure logged on row).
func (p *Poller) processOne(ctx context.Context, kind WorkKind, handler WorkHandler) error {
	// Step 1: query next pending row.
	row, err := p.fetchPending(ctx, kind)
	if err != nil {
		return err
	}
	if row == nil {
		// No pending row; nothing to do.
		return nil
	}

	// Step 2: CAS transition pending → in_progress.
	res, err := p.writeDB.ExecContext(ctx,
		`UPDATE post_promotion_queue SET state='in_progress', attempts=attempts+1 WHERE seq=? AND state='pending'`,
		row.Seq,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// Another goroutine grabbed it (shouldn't happen with one goroutine per kind).
		return nil
	}

	// Re-read attempts from DB after increment so our local Row reflects reality.
	row.Attempts++

	// Step 3: call handler.
	handlerErr := handler.Handle(ctx, *row)

	now := time.Now().Unix()
	if handlerErr == nil {
		// Step 4a: success.
		_, err = p.writeDB.ExecContext(ctx,
			`UPDATE post_promotion_queue SET state='done', completed_at=? WHERE seq=?`,
			now, row.Seq,
		)
		return err
	}

	// Step 4b: failure — re-queue or fail permanently.
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
	return err
}

// fetchPending queries the next pending row for the given work_kind.
// Returns nil, nil when no row is available.
func (p *Poller) fetchPending(ctx context.Context, kind WorkKind) (*Row, error) {
	r := &Row{}
	err := p.readDB.QueryRowContext(ctx, `
		SELECT seq, promotion_id, repo_id, branch, git_sha, work_kind, payload, state, attempts, enqueued_at
		FROM post_promotion_queue
		WHERE state='pending' AND work_kind=?
		ORDER BY seq
		LIMIT 1`,
		string(kind),
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
