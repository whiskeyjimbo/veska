// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"context"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// WikiRenderReport summarizes whether - and how long ago - the wiki was last
// rendered. It carries no staleness verdict: a never-rendered wiki and a
// freshly rendered one are both reported as Status "healthy" (the absence of
// a render is operational state, not a fault). Status "broken" is reserved
// for a probe failure (nil store or query error).
type WikiRenderReport struct {
	// Rendered is true once a successful render has been recorded.
	Rendered bool `json:"rendered"`
	// LastRenderAt is the timestamp of the most recent successful render;
	// the zero time when Rendered is false.
	LastRenderAt time.Time `json:"last_render_at"`
	// AgeSeconds is now-LastRenderAt in whole seconds; 0 when never rendered.
	AgeSeconds int64         `json:"age_seconds"`
	Status     health.Status `json:"status"`
}

// renderTimeReader is the minimal surface CheckWikiRender needs. Defined here
// (rather than imported from sqlite) so the doctor package stays
// unidirectional: production callers pass *sqlite.WikiRenderStateRepo, which
// satisfies this shape.
type renderTimeReader interface {
	LastRenderAt(ctx context.Context) (time.Time, bool, error)
}

// wikiRenderConfig holds the resolved options for CheckWikiRender.
type wikiRenderConfig struct {
	now func() time.Time
}

// Option configures CheckWikiRender.
type Option func(*wikiRenderConfig)

// WithClock replaces the wall-clock used to compute render age. Defaults to
// time.Now; tests inject a fixed clock for deterministic age assertions.
func WithClock(now func() time.Time) Option {
	return func(c *wikiRenderConfig) {
		if now != nil {
			c.now = now
		}
	}
}

// CheckWikiRender reads the last successful wiki-render timestamp and reports
// its age. A nil store or query failure yields Status "broken" with a nil
// error so callers can safely render the report. A never-rendered wiki yields
// Status "healthy" with Rendered false.
func CheckWikiRender(ctx context.Context, store renderTimeReader, opts ...Option) (WikiRenderReport, error) {
	if store == nil {
		return WikiRenderReport{Status: health.StatusBroken}, nil
	}

	cfg := wikiRenderConfig{now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	at, rendered, err := store.LastRenderAt(ctx)
	if err != nil {
		return WikiRenderReport{Status: health.StatusBroken}, nil
	}
	if !rendered {
		return WikiRenderReport{Rendered: false, Status: health.StatusHealthy}, nil
	}

	age := max(cfg.now().Sub(at), 0)
	return WikiRenderReport{
		Rendered:     true,
		LastRenderAt: at,
		AgeSeconds:   int64(age / time.Second),
		Status:       health.StatusHealthy,
	}, nil
}
