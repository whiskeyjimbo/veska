// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package doctor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

type fakeRenderState struct {
	at       time.Time
	rendered bool
	err      error
}

func (f *fakeRenderState) LastRenderAt(_ context.Context) (time.Time, bool, error) {
	if f.err != nil {
		return time.Time{}, false, f.err
	}
	return f.at, f.rendered, nil
}

// AC1: a render exists → doctor reports the age of the last successful render.
func TestCheckWikiRender_ReportsAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	last := now.Add(-90 * time.Minute)
	got, err := doctor.CheckWikiRender(context.Background(),
		&fakeRenderState{at: last, rendered: true},
		doctor.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Status != "healthy" {
		t.Errorf("status: want healthy, got %q", got.Status)
	}
	if !got.Rendered {
		t.Errorf("rendered: want true")
	}
	if got.AgeSeconds != 5400 {
		t.Errorf("age: want 5400s, got %d", got.AgeSeconds)
	}
	if !got.LastRenderAt.Equal(last) {
		t.Errorf("last render at: want %v, got %v", last, got.LastRenderAt)
	}
}

// AC2: no render yet → reported explicitly, status is non-error (healthy).
func TestCheckWikiRender_NeverRendered(t *testing.T) {
	t.Parallel()
	got, err := doctor.CheckWikiRender(context.Background(),
		&fakeRenderState{rendered: false})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Status != "healthy" {
		t.Errorf("status: want healthy (never rendered is not an error), got %q", got.Status)
	}
	if got.Rendered {
		t.Errorf("rendered: want false")
	}
	if got.AgeSeconds != 0 {
		t.Errorf("age: want 0 when never rendered, got %d", got.AgeSeconds)
	}
}

func TestCheckWikiRender_NilStore(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckWikiRender(context.Background(), nil)
	if got.Status != "broken" {
		t.Errorf("status: want broken on nil store, got %q", got.Status)
	}
}

func TestCheckWikiRender_QueryError(t *testing.T) {
	t.Parallel()
	got, _ := doctor.CheckWikiRender(context.Background(),
		&fakeRenderState{err: errors.New("db gone")})
	if got.Status != "broken" {
		t.Errorf("status: want broken on query error, got %q", got.Status)
	}
}
