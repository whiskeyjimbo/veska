// SPDX-License-Identifier: AGPL-3.0-only

package embedder

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Governor bounds how many embed calls the Worker may have in flight and
// learns from the outcome of each one. It is the control input to the drain
// loop: the loop embeds up to Limit() batches concurrently, and feeds every
// call's Outcome back so an adaptive implementation can retune.
//
// Two provider families want opposite things, which is why this is an
// interface and not a constant:
//
//   - Local (model2vec/static) and a single local Ollama instance serialize
//     embedding internally, so concurrency past 1 buys nothing; 1/latency is
//     the ceiling and a greedy drain reaches it. They use a fixed limit of 1.
//   - Hosted APIs (Anthropic, OpenAI) are network-round-trip bound and
//     parallelize up to an RPM/TPM quota, so concurrency IS the throughput
//     lever. They use an adaptive limit fed by Outcome latency, and honor an
//     explicit Outcome.RetryAfter from a 429.
//
// The adaptive (AIMD) implementation lands with the hosted-API provider work;
// this package ships the fixed-limit governor, which keeps Ollama behaving
// exactly like the prior serial drain (minus the inert rate limiter).
type Governor interface {
	// Acquire blocks until a slot is free or ctx is done. It also blocks
	// while a prior Outcome.RetryAfter backoff is in effect. The returned
	// Permit must be Released exactly once.
	Acquire(ctx context.Context) (Permit, error)

	// Limit reports the current concurrency ceiling. The drain loop reads it
	// to decide how many batches to embed per pass and to publish a gauge.
	Limit() int
}

// Permit is a single in-flight slot handed out by a Governor. Release returns
// the slot and reports what happened so the Governor can adapt.
type Permit interface {
	Release(Outcome)
}

// Outcome is the post-call feedback a Permit carries back to its Governor.
type Outcome struct {
	// Latency is the wall-clock duration of the embed call.
	Latency time.Duration
	// Err is the embed call's error (nil on success).
	Err error
	// RetryAfter, when > 0, is a server-instructed backoff (e.g. an HTTP 429
	// Retry-After). The Governor pauses ALL acquisitions until it elapses -
	// a hard signal an AIMD latency loop would otherwise discover slowly,
	// burning quota on rejected calls. No current provider populates it;
	// hosted-API adapters will via RetryAfterFromErr.
	RetryAfter time.Duration
}

// fixedGovernor caps concurrency at a fixed limit via a buffered-channel
// semaphore, and honors Outcome.RetryAfter with a global pause. It does NOT
// adapt the limit - that is the AIMD governor's job (deferred to the
// hosted-API work). At limit 1 it reproduces the prior serial drain.
type fixedGovernor struct {
	sem chan struct{}

	mu         sync.Mutex
	pauseUntil time.Time
}

// NewFixedGovernor returns a Governor with a static concurrency limit. A limit
// < 1 is clamped to 1.
func NewFixedGovernor(limit int) Governor {
	if limit < 1 {
		limit = 1
	}
	return &fixedGovernor{sem: make(chan struct{}, limit)}
}

func (g *fixedGovernor) Limit() int { return cap(g.sem) }

func (g *fixedGovernor) Acquire(ctx context.Context) (Permit, error) {
	// Honor an in-effect backoff before taking a slot. Re-checked in a loop
	// because a concurrent Release may extend the deadline while we wait.
	for {
		g.mu.Lock()
		wait := time.Until(g.pauseUntil)
		g.mu.Unlock()
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case g.sem <- struct{}{}:
		return &fixedPermit{g: g}, nil
	}
}

type fixedPermit struct {
	g    *fixedGovernor
	once sync.Once
}

func (p *fixedPermit) Release(o Outcome) {
	p.once.Do(func() {
		if o.RetryAfter > 0 {
			deadline := time.Now().Add(o.RetryAfter)
			p.g.mu.Lock()
			// Only extend, never shorten: a later, smaller Retry-After must
			// not undercut an earlier, longer backoff still in effect.
			if deadline.After(p.g.pauseUntil) {
				p.g.pauseUntil = deadline
			}
			p.g.mu.Unlock()
		}
		<-p.g.sem
	})
}

// retryAfterCarrier is the seam hosted-API adapters use to surface a 429
// Retry-After into Outcome.RetryAfter without this package importing any
// provider: a provider error implements it. No provider does yet.
type retryAfterCarrier interface {
	error
	RetryAfter() time.Duration
}

// RetryAfterFromErr extracts a server-instructed backoff from err, or 0 if
// none. Used by the Worker to populate Outcome.RetryAfter.
func RetryAfterFromErr(err error) time.Duration {
	if c, ok := errors.AsType[retryAfterCarrier](err); ok {
		return c.RetryAfter()
	}
	return 0
}
