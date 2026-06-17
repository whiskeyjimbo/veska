// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package reindexcmd

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
)

// spyReparserFactory returns a ReparserFactory whose reparser records each
// invocation. Used to assert the cold-scan path does NOT run when the daemon
// is up (the daemon's in-process reparser handles the scan instead).
func spyReparserFactory(calls *atomic.Int32) func(*sqlite.Pools, application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
	return func(_ *sqlite.Pools, _ application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
		return func(_ context.Context, _ application.RepoRecord) error {
			calls.Add(1)
			return nil
		}, nil
	}
}

// notCalledMatchByPath fails the test if the cwd→repo matcher is consulted
// the daemon-up dispatch path must not touch the local DB.
func notCalledMatchByPath(t *testing.T) func(context.Context, *sql.DB, string) (repo.Record, error) {
	return func(context.Context, *sql.DB, string) (repo.Record, error) {
		t.Helper()
		t.Fatal("MatchByPath must not be called on the daemon-dispatch path")
		return repo.Record{}, nil
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// TestRun_DispatchesViaMCPWhenDaemonUp pins: when the daemon is
// reachable, Run dispatches via DialReindex and never falls through to the
// direct sqlite path (which would race the daemon for the write lock). A
// cwd-invocation (empty Target) must send rootPath=cwd and repoID="".
func TestRun_DispatchesViaMCPWhenDaemonUp(t *testing.T) {
	repoRoot := t.TempDir()
	chdir(t, repoRoot)

	var reparserCalls atomic.Int32
	var dialCalls atomic.Int32
	var gotRepoID, gotRootPath string

	var buf bytes.Buffer
	err := Run(context.Background(), Params{
		Target:        "",
		Out:           &buf,
		ErrOut:        &buf,
		DaemonRunning: func() bool { return true },
		DialReindex: func(_ context.Context, rid, rp string) (string, error) {
			dialCalls.Add(1)
			gotRepoID, gotRootPath = rid, rp
			if rid != "" {
				return rid, nil
			}
			return "resolved-by-daemon", nil
		},
		ReparserFactory: spyReparserFactory(&reparserCalls),
		MatchByPath:     notCalledMatchByPath(t),
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if dialCalls.Load() != 1 {
		t.Errorf("DialReindex calls = %d, want 1; output: %s", dialCalls.Load(), buf.String())
	}
	if reparserCalls.Load() != 0 {
		t.Errorf("direct reparser must NOT run when daemon is up, got %d invocations", reparserCalls.Load())
	}
	if gotRootPath != repoRoot {
		t.Errorf("dial got rootPath = %q, want %q", gotRootPath, repoRoot)
	}
	if gotRepoID != "" {
		t.Errorf("dial got repoID = %q, want empty (cwd resolution)", gotRepoID)
	}
}

// TestRun_DispatchesViaMCPWithRepoIDArg confirms a non-path target is passed
// through as repo_id (the daemon resolves it).
func TestRun_DispatchesViaMCPWithRepoIDArg(t *testing.T) {
	const repoID = "abc123repoid"
	var gotRepoID string

	var buf bytes.Buffer
	err := Run(context.Background(), Params{
		Target:        repoID,
		Out:           &buf,
		ErrOut:        &buf,
		DaemonRunning: func() bool { return true },
		DialReindex: func(_ context.Context, rid, _ string) (string, error) {
			gotRepoID = rid
			return rid, nil
		},
		ReparserFactory: spyReparserFactory(new(atomic.Int32)),
		MatchByPath:     notCalledMatchByPath(t),
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if gotRepoID != repoID {
		t.Errorf("dial got repoID = %q, want %q", gotRepoID, repoID)
	}
}

// TestRun_NoStopItFirstError pins the regression: with the daemon up, Run must
// NOT emit the legacy "stop it first" error message anywhere in its output
// (AC1 of ).
func TestRun_NoStopItFirstError(t *testing.T) {
	chdir(t, t.TempDir())

	var buf bytes.Buffer
	err := Run(context.Background(), Params{
		Out:             &buf,
		ErrOut:          &buf,
		DaemonRunning:   func() bool { return true },
		DialReindex:     func(_ context.Context, _, _ string) (string, error) { return "r1", nil },
		ReparserFactory: spyReparserFactory(new(atomic.Int32)),
		MatchByPath:     notCalledMatchByPath(t),
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "stop it first") {
		t.Errorf("output must not contain legacy 'stop it first' message: %s", buf.String())
	}
}

// TestMergeTarget covers the positional/flag merge rule used by reindexCmd.
// The DoD for is that --repo behaves as an alias for the
// positional arg and the positional wins on conflict.
func TestMergeTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		args       []string
		flag       string
		wantTarget string
		wantStderr string
	}{
		{name: "both empty", wantTarget: ""},
		{name: "flag only", flag: "abcd1234", wantTarget: "abcd1234"},
		{name: "positional only", args: []string{"abcd1234"}, wantTarget: "abcd1234"},
		{name: "same value both", args: []string{"abcd1234"}, flag: "abcd1234", wantTarget: "abcd1234"},
		{
			name: "conflict: positional wins, note on stderr",
			args: []string{"abcd1234"}, flag: "deadbeef",
			wantTarget: "abcd1234",
			wantStderr: `reindex: positional arg "abcd1234" overrides --repo "deadbeef"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			got := MergeTarget(&stderr, tc.args, tc.flag)
			if got != tc.wantTarget {
				t.Fatalf("target: got %q want %q", got, tc.wantTarget)
			}
			gotErr := stderr.String()
			if tc.wantStderr == "" && gotErr != "" {
				t.Fatalf("unexpected stderr: %q", gotErr)
			}
			if tc.wantStderr != "" && !strings.Contains(gotErr, tc.wantStderr) {
				t.Fatalf("stderr: got %q want substring %q", gotErr, tc.wantStderr)
			}
		})
	}
}
