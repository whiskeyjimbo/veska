// Package vulnrefresh contains the daemon-owned goroutine that keeps the OSV
// advisory cache fresh, off the promotion hot path.
//
// Scope (M7 §3 task A4): lifecycle and scheduling only. The refresher depends
// on the ports.VulnSource interface and calls its Refresh — it owns no cache
// state and performs no scanning. Network egress is entirely the adapter's
// concern; this package only decides *when* Refresh runs.
//
// Run calls Refresh once immediately on entry (so a daemon start kicks a
// catch-up refresh) and then on every tick of a configurable interval. A
// Refresh error is logged and swallowed: a transient OSV.dev failure must not
// crash the daemon or stop the ticker — the next tick simply retries.
package vulnrefresh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// DefaultInterval is the refresh cadence when WithInterval is not supplied.
// OSV advisories change slowly; a daily refresh keeps the cache current
// without meaningful network cost.
const DefaultInterval = 24 * time.Hour

// ErrMissingDependency is returned by NewRefresher when the VulnSource is nil.
// It is errors.Is-matchable so callers can distinguish a wiring fault from a
// runtime failure.
var ErrMissingDependency = errors.New("vulnrefresh: missing required dependency")

// Refresher periodically invokes ports.VulnSource.Refresh. It carries no
// state beyond its collaborator and the interval; durability of the advisory
// cache lives entirely in the adapter.
type Refresher struct {
	source   ports.VulnSource
	interval time.Duration
}

// Option configures a Refresher.
type Option func(*Refresher)

// WithInterval overrides the refresh cadence (default 24h). Non-positive
// values are ignored so a misconfigured zero falls back to the default rather
// than producing a hot-spinning ticker.
func WithInterval(d time.Duration) Option {
	return func(r *Refresher) {
		if d > 0 {
			r.interval = d
		}
	}
}

// NewRefresher constructs a Refresher. The VulnSource is required: a nil
// source yields an error wrapping ErrMissingDependency and a nil *Refresher,
// surfacing the wiring fault at construction time rather than inside the
// goroutine.
func NewRefresher(source ports.VulnSource, opts ...Option) (*Refresher, error) {
	if source == nil {
		return nil, fmt.Errorf("vulnrefresh.NewRefresher: source is nil: %w", ErrMissingDependency)
	}
	r := &Refresher{
		source:   source,
		interval: DefaultInterval,
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Interval reports the resolved refresh cadence. Exposed for tests and for
// callers that want to log the effective schedule.
func (r *Refresher) Interval() time.Duration { return r.interval }

// Run blocks, refreshing the advisory cache once immediately and then on every
// tick of the configured interval. It returns when ctx is cancelled. A Refresh
// error is logged and swallowed; the ticker keeps running.
//
// Run is intended to be launched in its own goroutine by the daemon
// composition root.
func (r *Refresher) Run(ctx context.Context) {
	// Kick a catch-up refresh on entry so a freshly started daemon does not
	// wait a full interval for the first cache update.
	r.refresh(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh performs a single Refresh and isolates its error. A cancelled
// context is expected during shutdown and is not logged as a failure.
func (r *Refresher) refresh(ctx context.Context) {
	if err := r.source.Refresh(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("vulnrefresh: advisory cache refresh failed", "error", err)
	}
}
