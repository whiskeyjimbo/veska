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
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

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
	repoID = id
	// Resolve the canonical root that repo.Add stored, so the test can
	// chdir into it (repo.Add EvalSymlinks-canonicalises the path).
	rec, err := repo.Get(context.Background(), db, id)
	if err != nil || rec.RepoID == "" {
		t.Fatalf("repo.Get: %v (rec=%+v)", err, rec)
	}
	return rec.RootPath, id
}

// installSpyReparser swaps the reparser factory for a closure that records
// each invocation. The previous factory is restored when the test finishes.
func installSpyReparser(t *testing.T, calls *atomic.Int32, lastRepo *application.RepoRecord) {
	t.Helper()
	prev := reparserFactory
	reparserFactory = func(_ *sqlite.Pools, _ application.IgnoreLoader) (func(context.Context, application.RepoRecord) error, error) {
		return func(_ context.Context, rec application.RepoRecord) error {
			calls.Add(1)
			if lastRepo != nil {
				*lastRepo = rec
			}
			return nil
		}, nil
	}
	t.Cleanup(func() { reparserFactory = prev })
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
	installSpyReparser(t, &calls, &got)

	chdir(t, repoRoot)

	var out bytes.Buffer
	root := newRootCmd()
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
	installSpyReparser(t, &calls, &got)

	var out bytes.Buffer
	root := newRootCmd()
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
	db, err := sql.Open("sqlite", dbPath)
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
	installSpyReparser(t, &calls, nil)

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex", repoID})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex: %v\n%s", err, buf.String())
	}
	if calls.Load() != 1 {
		t.Errorf("reparser invocations at-HEAD: got %d want 1", calls.Load())
	}
	_ = repoRoot
	_ = time.Now
}

// TestReindexCmd_DispatchesViaMCPWhenDaemonUp pins solov2-4d7b: when the
// daemon socket is reachable, reindex dispatches via the eng_reindex_repo
// MCP tool and never falls through to the direct sqlite path (which would
// race the daemon for the write lock). Before this change, reindex printed
// "stop it first" instead of using the daemon.
func TestReindexCmd_DispatchesViaMCPWhenDaemonUp(t *testing.T) {
	repoRoot, repoID := setupReindexEnv(t)

	// Spy reparser must NOT be called when the daemon is up — the daemon's
	// in-process reparser handles the scan instead.
	var reparserCalls atomic.Int32
	installSpyReparser(t, &reparserCalls, nil)

	// Simulate daemon-up.
	prevProbe := reindexDaemonProbe
	reindexDaemonProbe = func() bool { return true }
	t.Cleanup(func() { reindexDaemonProbe = prevProbe })

	// Spy dial that records the params it would have sent.
	var dialCalls atomic.Int32
	var gotRepoID, gotRootPath string
	prevDial := dialReindex
	dialReindex = func(_ context.Context, rid, rp string) (string, error) {
		dialCalls.Add(1)
		gotRepoID = rid
		gotRootPath = rp
		// Echo back the repo_id like a real daemon does.
		if rid != "" {
			return rid, nil
		}
		return repoID, nil
	}
	t.Cleanup(func() { dialReindex = prevDial })

	chdir(t, repoRoot)

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex: %v\n%s", err, buf.String())
	}
	if dialCalls.Load() != 1 {
		t.Errorf("dialReindex calls = %d, want 1; output: %s", dialCalls.Load(), buf.String())
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

// TestReindexCmd_DispatchesViaMCPWithRepoIDArg confirms the dispatch fork
// passes a non-path argument through as repo_id (the daemon resolves it).
func TestReindexCmd_DispatchesViaMCPWithRepoIDArg(t *testing.T) {
	_, repoID := setupReindexEnv(t)

	prevProbe := reindexDaemonProbe
	reindexDaemonProbe = func() bool { return true }
	t.Cleanup(func() { reindexDaemonProbe = prevProbe })

	var gotRepoID string
	prevDial := dialReindex
	dialReindex = func(_ context.Context, rid, _ string) (string, error) {
		gotRepoID = rid
		return rid, nil
	}
	t.Cleanup(func() { dialReindex = prevDial })

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex", repoID})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex %s: %v\n%s", repoID, err, buf.String())
	}
	if gotRepoID != repoID {
		t.Errorf("dial got repoID = %q, want %q", gotRepoID, repoID)
	}
}

// TestReindexCmd_NoStopItFirstError pins the regression: with the daemon up,
// reindex must NOT return the legacy "stop it first" error message anywhere
// in its output. (AC1 of solov2-4d7b.)
func TestReindexCmd_NoStopItFirstError(t *testing.T) {
	repoRoot, _ := setupReindexEnv(t)

	prevProbe := reindexDaemonProbe
	reindexDaemonProbe = func() bool { return true }
	t.Cleanup(func() { reindexDaemonProbe = prevProbe })

	prevDial := dialReindex
	dialReindex = func(_ context.Context, _, _ string) (string, error) { return "r1", nil }
	t.Cleanup(func() { dialReindex = prevDial })

	chdir(t, repoRoot)

	var buf bytes.Buffer
	root := newRootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"reindex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("veska reindex: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "stop it first") {
		t.Errorf("output must not contain legacy 'stop it first' message: %s", buf.String())
	}
}

func TestReindexCmd_UnknownRepo(t *testing.T) {
	setupReindexEnv(t)
	var calls atomic.Int32
	installSpyReparser(t, &calls, nil)

	var buf bytes.Buffer
	root := newRootCmd()
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
