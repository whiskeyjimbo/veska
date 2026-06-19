// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// These tests exercise the direct-SQLite cold-scan path end-to-end through
// newRootCmd: the daemon socket is absent in the test environment so
// daemonRunning returns false, and the cmd-owned reparserFactory seam is
// swapped for a spy. The daemon-dispatch fork lives in internal/cli/reindexcmd
// and is tested there (it needs the DaemonRunning seam).

// setupReindexEnv creates a temp VESKA_HOME with a migrated veska.db and a
// single registered git-backed repo. The repo is registered via repo.Add so
// the canonicalised path and generated id mirror production.
func setupReindexEnv(t *testing.T) (repoRoot, repoID string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("VESKA_HOME", home)

	repoRoot = t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	dbPath := filepath.Join(home, "veska.db")
	db, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id, _, err := repo.Add(context.Background(), db, repoRoot)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	// Resolve the canonical root that repo.Add stored, so the test can
	// chdir into it (repo.Add EvalSymlinks-canonicalises the path).
	rec, err := repo.Get(context.Background(), db, id)
	if err != nil || rec.RepoID == "" {
		t.Fatalf("repo.Get: %v (rec=%+v)", err, rec)
	}
	return rec.RootPath, id
}

// spyReparserFactory returns a reparser factory whose reparser records each
// invocation. It is injected into newRootCmd via withReparserFactory so the
// command runs the spy instead of standing up the real ingester/promoter
// pipeline.
func spyReparserFactory(calls *atomic.Int32, lastRepo *application.RepoRecord) reparserFactoryFunc {
	return func(_ *sqlite.Pools, _ application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
		return func(_ context.Context, rec application.RepoRecord) error {
			calls.Add(1)
			if lastRepo != nil {
				*lastRepo = rec
			}
			return nil
		}, nil
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

func TestReindexCmd_ResolvesByCwd(t *testing.T) {
	repoRoot, repoID := setupReindexEnv(t)
	var calls atomic.Int32
	var got application.RepoRecord

	chdir(t, repoRoot)

	var out bytes.Buffer
	root := newRootCmd(withReparserFactory(spyReparserFactory(&calls, &got)))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"reindex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex: %v\n%s", err, out.String())
	}
	if calls.Load() != 1 {
		t.Errorf("reparser invocations: got %d want 1", calls.Load())
	}
	if got.RepoID != repoID {
		t.Errorf("repo passed to reparser: got %q want %q", got.RepoID, repoID)
	}
	if !strings.Contains(out.String(), repoID) {
		t.Errorf("output should mention repo id %q, got: %s", repoID, out.String())
	}
}

func TestReindexCmd_ResolvesByID(t *testing.T) {
	_, repoID := setupReindexEnv(t)
	var calls atomic.Int32
	var got application.RepoRecord

	var out bytes.Buffer
	root := newRootCmd(withReparserFactory(spyReparserFactory(&calls, &got)))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"reindex", repoID})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex %s: %v\n%s", repoID, err, out.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("reparser invocations: got %d want 1", calls.Load())
	}
	if got.RepoID != repoID {
		t.Errorf("repo passed to reparser: got %q want %q", got.RepoID, repoID)
	}
}

func TestReindexCmd_ForcesReparseAtHEAD(t *testing.T) {
	repoRoot, repoID := setupReindexEnv(t)
	// Pre-set last_promoted_sha = HEAD so a daemon's StartupResync would skip
	// this repo. The CLI must invoke the reparser anyway.
	out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	head := strings.TrimSpace(string(out))

	dbPath := filepath.Join(os.Getenv("VESKA_HOME"), "veska.db")
	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE repos SET last_promoted_sha = ? WHERE repo_id = ?`, head, repoID,
	); err != nil {
		t.Fatalf("update last_promoted_sha: %v", err)
	}

	var calls atomic.Int32

	var buf bytes.Buffer
	root := newRootCmd(withReparserFactory(spyReparserFactory(&calls, nil)))
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex", repoID})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex: %v\n%s", err, buf.String())
	}
	if calls.Load() != 1 {
		t.Errorf("reparser invocations at-HEAD: got %d want 1", calls.Load())
	}
}

func TestReindexCmd_UnknownRepo(t *testing.T) {
	setupReindexEnv(t)
	var calls atomic.Int32

	var buf bytes.Buffer
	root := newRootCmd(withReparserFactory(spyReparserFactory(&calls, nil)))
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex", "no-such-repo-id"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unregistered repo, got nil")
	}
	if calls.Load() != 0 {
		t.Errorf("reparser must not run on unknown repo, got %d invocations", calls.Load())
	}
}

// TestReindexCmd_RepoFlagListed asserts --repo appears in the command's flag
// set (parity with deps/findings/calls/blast/search).
func TestReindexCmd_RepoFlagListed(t *testing.T) {
	t.Parallel()
	c := reindexCmd(defaultReparserFactory)
	if f := c.Flags().Lookup("repo"); f == nil {
		t.Fatal("reindex command should expose --repo flag for parity with siblings")
	}
}
