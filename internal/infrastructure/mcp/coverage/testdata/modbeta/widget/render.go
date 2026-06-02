// Package widget renders themed widgets for the beta dashboard.
//
// The vocabulary here (widget, render, palette, theme, badge) is disjoint
// from modalpha's metric/series vocabulary on purpose.
package widget

import "example.com/modalpha/metric"

// Palette names the colour theme a widget renders with.
type Palette struct {
	Theme string
}

// Renderer draws a widget body into a string buffer.
type Renderer interface {
	Render() string
}

// Badge is a small labelled widget. RenderBadge formats it for display.
type Badge struct {
	Label   string
	Palette Palette
}

// RenderBadge formats the badge label with its palette theme. It calls into
// modalpha.metric.ComputeVariance to derive a jitter factor — this is the
// genuine cross-module call that produces a cross-repo edge fact.
func (b Badge) RenderBadge(samples []float64) string {
	jitter := metric.ComputeVariance(metric.Series{Samples: samples})
	return b.Palette.Theme + ":" + b.Label + formatJitter(jitter)
}

// formatJitter is a within-module helper (local CALLS edge target).
func formatJitter(v float64) string {
	if v > 0 {
		return "*"
	}
	return ""
}
