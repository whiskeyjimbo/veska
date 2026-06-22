// SPDX-License-Identifier: AGPL-3.0-only

// Package pollloop drives the greedy-then-idle drain loop shared by the
// post-promotion queue poller and the embedder worker. Both poll a backlog
// that arrives in bursts after a promotion: the right behavior is to drain
// back-to-back while work remains and only sleep an idle interval once a pass
// comes up empty. Sleeping a full interval between every unit of work caps
// drain throughput at one unit per interval regardless of how fast the host
// can do the work - the bug that 1e9c751 fixed for the embedder and that this
// helper keeps the queue poller from re-introducing.
package pollloop

import (
	"context"
	"time"
)

// Run drives step greedily until it reports no work (or ctx is canceled), then
// waits interval before polling again. step returns true when it did work and
// should be called again immediately, false to go idle until the next tick.
// Backpressure (e.g. yielding the write lock during a cold scan) is step's
// responsibility: return false while paused so Run idles rather than spins.
func Run(ctx context.Context, interval time.Duration, step func(context.Context) bool) {
	// First tick fires immediately so a backlog present at startup drains at
	// once rather than after an initial idle interval.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		for step(ctx) {
			if ctx.Err() != nil {
				return
			}
		}

		timer.Reset(interval)
	}
}
