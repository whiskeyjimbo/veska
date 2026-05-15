package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// testConfig returns a Config whose paths live under a fresh tmp dir so each
// test is hermetic (no ~/.veska state shared across runs).
func testConfig(t *testing.T) Config {
	t.Helper()
	home := t.TempDir()
	return Config{
		VeskaHome:     home,
		SQLitePath:    filepath.Join(home, "veska.db"),
		CLISockPath:   filepath.Join(home, "cli.sock"),
		MCPSockPath:   filepath.Join(home, "mcp.sock"),
		VectorBackend: vector.BackendSQLiteVec,
		OllamaURL:     "http://127.0.0.1:0", // unreachable; not dialed at construct time
		EmbedModel:    "nomic-embed-text",
	}
}

// TestWire_Constructs verifies newDaemon assembles the full collaborator
// graph from a minimal but valid config without error or panic.
func TestWire_Constructs(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("newDaemon returned nil daemon")
	}
	t.Cleanup(func() { _ = d.Stop() })
}

// TestWire_UnknownVectorBackend ensures an invalid VESKA_VECTOR_BACKEND value
// surfaces as a typed *ErrMissingDep rather than as a generic open error.
func TestWire_UnknownVectorBackend(t *testing.T) {
	cfg := testConfig(t)
	cfg.VectorBackend = "not-a-real-backend"
	_, err := newDaemon(cfg)
	if err == nil {
		t.Fatal("expected error for unknown vector backend")
	}
	var miss *ErrMissingDep
	if !errors.As(err, &miss) {
		t.Fatalf("want *ErrMissingDep, got %T: %v", err, err)
	}
	if miss.Name != "vector_backend" {
		t.Fatalf("missing dep name = %q; want %q", miss.Name, "vector_backend")
	}
}

// TestWire_HonorsEnvVectorBackend ensures the VESKA_VECTOR_BACKEND env var is
// resolved when Config.VectorBackend is empty.
func TestWire_HonorsEnvVectorBackend(t *testing.T) {
	t.Setenv("VESKA_VECTOR_BACKEND", string(vector.BackendSQLiteVec))
	cfg := testConfig(t)
	cfg.VectorBackend = "" // force env path
	resolved := ResolveConfig(cfg)
	if resolved.VectorBackend != vector.BackendSQLiteVec {
		t.Fatalf("VectorBackend = %q; want %q", resolved.VectorBackend, vector.BackendSQLiteVec)
	}
}

// TestErrMissingDep_Format formats both with and without the Why field so
// operators see a useful message in either path.
func TestErrMissingDep_Format(t *testing.T) {
	got := (&ErrMissingDep{Name: "x"}).Error()
	if got == "" {
		t.Fatal("empty error string")
	}
	got = (&ErrMissingDep{Name: "x", Why: "because"}).Error()
	if got == "" || !contains(got, "because") {
		t.Fatalf("Error() = %q; want it to contain Why", got)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestWire_StartStop verifies Start creates the sockets and Stop removes them.
// Both are exercised under a short bounded timeout to catch goroutine leaks.
func TestWire_StartStop(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// CLI + MCP sockets should be present.
	for _, p := range []string{cfg.CLISockPath, cfg.MCPSockPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected socket %q to exist after Start: %v", p, err)
		}
	}

	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Sockets should be cleaned up.
	for _, p := range []string{cfg.CLISockPath, cfg.MCPSockPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected socket %q to be removed after Stop; stat err=%v", p, err)
		}
	}
}

// TestWire_StartStopIdempotent verifies calling Start / Stop more than once
// does not deadlock or panic.
func TestWire_StartStopIdempotent(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := d.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestResolveConfig_AppliesDefaults confirms that an empty Config picks up
// every env-or-fallback default so newDaemon never sees a zero field.
func TestResolveConfig_AppliesDefaults(t *testing.T) {
	t.Setenv("VESKA_HOME", t.TempDir())
	t.Setenv("VESKA_VECTOR_BACKEND", "")
	t.Setenv("VESKA_OLLAMA_URL", "")
	t.Setenv("VESKA_EMBED_MODEL", "")

	got := ResolveConfig(Config{})
	if got.VeskaHome == "" {
		t.Error("VeskaHome empty after resolve")
	}
	if got.SQLitePath == "" {
		t.Error("SQLitePath empty after resolve")
	}
	if got.CLISockPath == "" {
		t.Error("CLISockPath empty after resolve")
	}
	if got.MCPSockPath == "" {
		t.Error("MCPSockPath empty after resolve")
	}
	if got.VectorBackend != vector.BackendSQLiteVec {
		t.Errorf("VectorBackend = %q; want %q", got.VectorBackend, vector.BackendSQLiteVec)
	}
	if got.EmbedModel != "nomic-embed-text" {
		t.Errorf("EmbedModel = %q; want %q", got.EmbedModel, "nomic-embed-text")
	}
	if got.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q; want default", got.OllamaURL)
	}
}

// TestWire_StartWatchesRegisteredRepos verifies that a repo registered in the
// daemon's SQLite repos table before Start is added to the fsnotify watcher.
func TestWire_StartWatchesRegisteredRepos(t *testing.T) {
	cfg := testConfig(t)
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop() })

	// Register a repo before Start.
	gitDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gitDir, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	repoID, err := repo.Add(context.Background(), d.pools.WriteHot, gitDir)
	if err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	watched := d.watcher.WatchedRepoIDs()
	found := false
	for _, id := range watched {
		if id == repoID {
			found = true
		}
	}
	if !found {
		t.Errorf("watcher does not watch registered repo %q; watched=%v", repoID, watched)
	}
}
