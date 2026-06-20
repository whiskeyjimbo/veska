// SPDX-License-Identifier: AGPL-3.0-only

package doctor

import (
	"context"
	"errors"
	"testing"
)

// stubTokensReader is a fixed-value tokensTodayReader for the pipelines probe.
type stubTokensReader struct {
	tokens int
	err    error
}

func (s stubTokensReader) TokensToday(context.Context) (int, error) {
	return s.tokens, s.err
}

// TestCheckPipelines_Healthy proves usage below the daily cap is healthy.
func TestCheckPipelines_Healthy(t *testing.T) {
	t.Parallel()
	rep, err := CheckPipelines(context.Background(), stubTokensReader{tokens: 1000}, 5000, 100000)
	if err != nil {
		t.Fatalf("CheckPipelines: %v", err)
	}
	if rep.Status != "healthy" {
		t.Errorf("status = %q, want healthy", rep.Status)
	}
	if rep.TokensToday != 1000 || rep.MaxTokensPerDay != 5000 || rep.MaxTokensPerCommit != 100000 {
		t.Errorf("report = %+v, want tokens_today=1000 caps 5000/100000", rep)
	}
	if rep.Paused {
		t.Error("paused = true, want false below the cap")
	}
}

// TestCheckPipelines_DegradedWhenPaused proves reaching the daily cap reports
// degraded + paused.
func TestCheckPipelines_DegradedWhenPaused(t *testing.T) {
	t.Parallel()
	rep, err := CheckPipelines(context.Background(), stubTokensReader{tokens: 5000}, 5000, 100000)
	if err != nil {
		t.Fatalf("CheckPipelines: %v", err)
	}
	if rep.Status != "degraded" {
		t.Errorf("status = %q, want degraded", rep.Status)
	}
	if !rep.Paused {
		t.Error("paused = false, want true at the cap")
	}
}

// TestCheckPipelines_ZeroCapUnlimited proves a daily cap of 0 never degrades.
func TestCheckPipelines_ZeroCapUnlimited(t *testing.T) {
	t.Parallel()
	rep, err := CheckPipelines(context.Background(), stubTokensReader{tokens: 9_999_999}, 0, 0)
	if err != nil {
		t.Fatalf("CheckPipelines: %v", err)
	}
	if rep.Status != "healthy" || rep.Paused {
		t.Errorf("report = %+v, want healthy/not-paused with cap 0", rep)
	}
}

// TestCheckPipelines_BrokenOnReadError proves a read failure yields broken.
func TestCheckPipelines_BrokenOnReadError(t *testing.T) {
	t.Parallel()
	rep, err := CheckPipelines(context.Background(), stubTokensReader{err: errors.New("db down")}, 5000, 0)
	if err != nil {
		t.Fatalf("CheckPipelines returned error, want nil: %v", err)
	}
	if rep.Status != "broken" {
		t.Errorf("status = %q, want broken", rep.Status)
	}
}

// TestCheckPipelines_NilReader proves a nil reader yields broken safely.
func TestCheckPipelines_NilReader(t *testing.T) {
	t.Parallel()
	rep, err := CheckPipelines(context.Background(), nil, 5000, 0)
	if err != nil {
		t.Fatalf("CheckPipelines: %v", err)
	}
	if rep.Status != "broken" {
		t.Errorf("status = %q, want broken", rep.Status)
	}
}
